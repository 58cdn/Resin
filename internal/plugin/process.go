package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const (
	processRegisterTimeout = 10 * time.Second
	processShutdownTimeout = 3 * time.Second
	processMaxLineBytes    = 16 << 20
	processMinBackoff      = time.Second
	processMaxBackoff      = 30 * time.Second
	processStableAfter     = 30 * time.Second
)

var (
	errProcessExited     = errors.New("plugin process exited")
	errProcessRestarting = errors.New("plugin process is restarting")
	errProcessClosed     = errors.New("plugin process is stopped")
)

// processSpec is everything needed to (re)start a package plugin process.
type processSpec struct {
	ID           string
	Dir          string
	DataDir      string
	Runtime      pluginsdk.RuntimeSpec
	Declared     pluginsdk.Capabilities
	ResinVersion string
	Logf         func(format string, args ...any)
	// OnStatus is called when the process crashes or is restarted.
	OnStatus func(status, lastError string)
	// OnRestart is called after each successful automatic restart.
	OnRestart func()
}

// processInstance supervises one plugin process and restarts it when it
// exits unexpectedly. Calls made while it is restarting fail fast.
type processInstance struct {
	spec processSpec

	mu     sync.Mutex
	client *rpcClient
	config json.RawMessage
	caps   pluginsdk.Capabilities
	closed bool

	closeCh chan struct{}
	doneCh  chan struct{}
}

func startProcess(ctx context.Context, spec processSpec, config json.RawMessage) (*processInstance, error) {
	client, caps, err := spawnAndRegister(ctx, spec, config)
	if err != nil {
		return nil, err
	}
	p := &processInstance{
		spec:    spec,
		client:  client,
		config:  config,
		caps:    caps,
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go p.supervise(client)
	return p, nil
}

func (p *processInstance) current() (*rpcClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errProcessClosed
	}
	if p.client == nil {
		return nil, errProcessRestarting
	}
	return p.client, nil
}

func (p *processInstance) Capabilities() pluginsdk.Capabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps
}

func (p *processInstance) Configure(ctx context.Context, config json.RawMessage) error {
	c, err := p.current()
	if err != nil {
		return err
	}
	if err := c.call(ctx, pluginsdk.MethodConfigure, pluginsdk.ConfigureParams{Config: config}, nil); err != nil {
		return err
	}
	p.mu.Lock()
	p.config = config
	p.mu.Unlock()
	return nil
}

func (p *processInstance) Inspect(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	c, err := p.current()
	if err != nil {
		return nil, err
	}
	var dec pluginsdk.RequestDecision
	if err := c.call(ctx, pluginsdk.MethodInspectRequest, req, &dec); err != nil {
		return nil, err
	}
	return &dec, nil
}

func (p *processInstance) HandleEvents(ctx context.Context, events []pluginsdk.Event) error {
	c, err := p.current()
	if err != nil {
		return err
	}
	return c.call(ctx, pluginsdk.MethodEventBatch, pluginsdk.EventBatchParams{Events: events}, nil)
}

func (p *processInstance) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.doneCh
		return nil
	}
	p.closed = true
	client := p.client
	p.client = nil
	close(p.closeCh)
	p.mu.Unlock()

	if client != nil {
		client.shutdown(ctx)
	}
	<-p.doneCh
	return nil
}

func (p *processInstance) supervise(client *rpcClient) {
	defer close(p.doneCh)
	backoff := processMinBackoff
	for {
		select {
		case <-client.exited:
		case <-p.closeCh:
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.client = nil
		p.mu.Unlock()

		reason := client.exitReason()
		p.spec.Logf("[plugin %s] process exited: %v; restarting", p.spec.ID, reason)
		if p.spec.OnStatus != nil {
			p.spec.OnStatus(StatusStarting, fmt.Sprintf("process exited: %v", reason))
		}
		if time.Since(client.startedAt) >= processStableAfter {
			backoff = processMinBackoff
		}

		for {
			select {
			case <-time.After(backoff):
			case <-p.closeCh:
				return
			}
			backoff = min(backoff*2, processMaxBackoff)

			p.mu.Lock()
			config := p.config
			p.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), processRegisterTimeout)
			next, caps, err := spawnAndRegister(ctx, p.spec, config)
			cancel()
			if err != nil {
				p.spec.Logf("[plugin %s] restart failed: %v", p.spec.ID, err)
				if p.spec.OnStatus != nil {
					p.spec.OnStatus(StatusError, err.Error())
				}
				continue
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				next.shutdown(context.Background())
				return
			}
			p.client = next
			p.caps = caps
			p.mu.Unlock()
			client = next
			if p.spec.OnStatus != nil {
				p.spec.OnStatus(StatusRunning, "")
			}
			if p.spec.OnRestart != nil {
				p.spec.OnRestart()
			}
			break
		}
	}
}

// spawnAndRegister starts the process and completes plugin.register.
func spawnAndRegister(ctx context.Context, spec processSpec, config json.RawMessage) (*rpcClient, pluginsdk.Capabilities, error) {
	client, err := spawnProcess(spec)
	if err != nil {
		return nil, pluginsdk.Capabilities{}, err
	}
	regCtx, cancel := context.WithTimeout(ctx, processRegisterTimeout)
	defer cancel()
	var result pluginsdk.RegisterResult
	err = client.call(regCtx, pluginsdk.MethodRegister, pluginsdk.RegisterParams{
		SchemaVersion: pluginsdk.SchemaVersion,
		ResinVersion:  spec.ResinVersion,
		PluginID:      spec.ID,
		DataDir:       spec.DataDir,
		Config:        config,
	}, &result)
	if err != nil {
		client.kill()
		return nil, pluginsdk.Capabilities{}, fmt.Errorf("register: %w", err)
	}
	if result.SchemaVersion != 0 && result.SchemaVersion != pluginsdk.SchemaVersion {
		client.shutdown(ctx)
		return nil, pluginsdk.Capabilities{}, fmt.Errorf("register: unsupported schema_version %d", result.SchemaVersion)
	}
	return client, effectiveCapabilities(spec.Declared, result.Capabilities), nil
}

// resolveCommand maps argv[0] from the manifest to an executable path.
func resolveCommand(dir, name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		full := filepath.Join(dir, filepath.FromSlash(name))
		rel, err := filepath.Rel(dir, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("command %q escapes the plugin directory", name)
		}
		if runtime.GOOS == "windows" && filepath.Ext(full) == "" {
			if _, err := os.Stat(full + ".exe"); err == nil {
				full += ".exe"
			}
		}
		if _, err := os.Stat(full); err != nil {
			return "", fmt.Errorf("command %q not found in plugin package", name)
		}
		return full, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("command %q not found in PATH", name)
	}
	return path, nil
}

// pluginEnv builds the child environment. Resin's own RESIN_* variables
// (which include admin and proxy tokens) are never inherited.
func pluginEnv(spec processSpec) []string {
	env := make([]string, 0, len(os.Environ())+len(spec.Runtime.Env)+3)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(key), "RESIN_") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range spec.Runtime.Env {
		if strings.HasPrefix(strings.ToUpper(k), "RESIN_") {
			continue
		}
		env = append(env, k+"="+v)
	}
	return append(env,
		"RESIN_PLUGIN_ID="+spec.ID,
		"RESIN_PLUGIN_DIR="+spec.Dir,
		"RESIN_PLUGIN_DATA_DIR="+spec.DataDir,
	)
}

func spawnProcess(spec processSpec) (*rpcClient, error) {
	if len(spec.Runtime.Command) == 0 {
		return nil, errors.New("empty runtime command")
	}
	path, err := resolveCommand(spec.Dir, spec.Runtime.Command[0])
	if err != nil {
		return nil, err
	}
	if spec.DataDir != "" {
		if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	cmd := exec.Command(path, spec.Runtime.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = pluginEnv(spec)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(path), err)
	}
	c := &rpcClient{
		id:        spec.ID,
		cmd:       cmd,
		stdin:     stdin,
		pending:   make(map[int64]chan rpcResult),
		readDone:  make(chan struct{}),
		exited:    make(chan struct{}),
		startedAt: time.Now(),
		logf:      spec.Logf,
	}
	var pipes sync.WaitGroup
	pipes.Add(2)
	go func() {
		defer pipes.Done()
		c.readLoop(stdout)
	}()
	go func() {
		defer pipes.Done()
		c.logStderr(stderr)
	}()
	go func() {
		pipes.Wait()
		err := cmd.Wait()
		c.setExitErr(err)
		close(c.exited)
	}()
	return c, nil
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// rpcClient is the host side of one plugin process connection.
type rpcClient struct {
	id        string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt time.Time
	logf      func(format string, args ...any)

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan rpcResult
	readErr  error
	exitErr  error
	readDone chan struct{}
	exited   chan struct{}

	shutdownOnce sync.Once
}

func (c *rpcClient) call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcResult, 1)

	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return errProcessExited
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg := pluginsdk.Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(strconv.FormatInt(id, 10)),
		Method:  method,
		Params:  raw,
	}
	if err := c.write(msg); err != nil {
		c.forget(id)
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 {
			if err := json.Unmarshal(res.result, result); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case <-c.readDone:
		c.forget(id)
		// A response may have raced with EOF.
		select {
		case res := <-ch:
			if res.err == nil && result != nil && len(res.result) > 0 {
				_ = json.Unmarshal(res.result, result)
			}
			return res.err
		default:
		}
		return errProcessExited
	}
}

func (c *rpcClient) forget(id int64) {
	c.mu.Lock()
	if c.pending != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *rpcClient) write(msg pluginsdk.Message) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(line); err != nil {
		return fmt.Errorf("%w: %v", errProcessExited, err)
	}
	return nil
}

func (c *rpcClient) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 64<<10)
	var loopErr error
	for {
		line, err := readProtocolLine(reader)
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				loopErr = err
				// Unrecoverable framing error: stop the process.
				c.kill()
			}
			break
		}
	}
	c.mu.Lock()
	c.readErr = loopErr
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcResult{err: errProcessExited}
	}
	close(c.readDone)
	// Keep draining so a misbehaving process never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
}

func (c *rpcClient) handleLine(line []byte) {
	var msg pluginsdk.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		c.logf("[plugin %s] invalid protocol line: %v", c.id, err)
		return
	}
	if msg.Method != "" {
		// Plugins cannot call the host in this protocol version.
		if len(msg.ID) > 0 && string(msg.ID) != "null" {
			_ = c.write(pluginsdk.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &pluginsdk.RPCError{Code: pluginsdk.CodeMethodNotFound, Message: "method not found: " + msg.Method},
			})
		}
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(msg.ID)), 10, 64)
	if err != nil {
		if msg.Error != nil {
			c.logf("[plugin %s] protocol error: %s", c.id, msg.Error.Message)
		}
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	if ch != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ch == nil {
		return // late response to a timed-out call
	}
	if msg.Error != nil {
		ch <- rpcResult{err: fmt.Errorf("plugin error: %s", msg.Error.Message)}
		return
	}
	ch <- rpcResult{result: msg.Result}
}

func readProtocolLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > processMaxLineBytes {
			return nil, fmt.Errorf("protocol line exceeds %d bytes", processMaxLineBytes)
		}
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

func (c *rpcClient) logStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		c.logf("[plugin %s] %s", c.id, scanner.Text())
	}
	_, _ = io.Copy(io.Discard, r)
}

func (c *rpcClient) setExitErr(err error) {
	c.mu.Lock()
	c.exitErr = err
	c.mu.Unlock()
}

func (c *rpcClient) exitReason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	if c.exitErr != nil {
		return c.exitErr
	}
	return errProcessExited
}

func (c *rpcClient) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// shutdown asks the process to exit, then closes stdin and finally kills it
// if it does not exit in time. It waits until the process is gone.
func (c *rpcClient) shutdown(ctx context.Context) {
	c.shutdownOnce.Do(func() {
		sdCtx, cancel := context.WithTimeout(ctx, processShutdownTimeout)
		_ = c.call(sdCtx, pluginsdk.MethodShutdown, struct{}{}, nil)
		cancel()
		c.writeMu.Lock()
		_ = c.stdin.Close()
		c.writeMu.Unlock()
		select {
		case <-c.exited:
			return
		case <-time.After(processShutdownTimeout):
		}
		c.kill()
	})
	<-c.exited
}
