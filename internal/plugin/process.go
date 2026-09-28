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
	"slices"
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
	// processExitWait bounds every wait for a killed process to be reaped.
	processExitWait = 5 * time.Second
	// processDrainTimeout is how long stdout/stderr are still read after the
	// process exits. Descendants that inherited them cannot keep the plugin
	// "running" past that.
	processDrainTimeout = time.Second
	processMaxLineBytes = 16 << 20
	processMaxLogLine   = 64 << 10
	// processWriteQueue is the number of messages buffered for the stdin
	// writer. Callers wait at most until their context is done.
	processWriteQueue  = 256
	processMinBackoff  = time.Second
	processMaxBackoff  = 30 * time.Second
	processStableAfter = 30 * time.Second
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
	// ctx is cancelled by Close; it also aborts a restart in progress.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	client *rpcClient
	config json.RawMessage
	// caps are negotiated by the first registration and kept across
	// restarts: the manager built the chain and event queue from them.
	caps   pluginsdk.Capabilities
	closed bool

	doneCh chan struct{}
}

func startProcess(ctx context.Context, spec processSpec, config json.RawMessage) (*processInstance, error) {
	client, caps, err := spawnAndRegister(ctx, spec, config)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(context.Background())
	p := &processInstance{
		spec:   spec,
		ctx:    pctx,
		cancel: cancel,
		client: client,
		config: config,
		caps:   caps,
		doneCh: make(chan struct{}),
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
	p.cancel()
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
		case <-p.ctx.Done():
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
			case <-p.ctx.Done():
				return
			}
			backoff = min(backoff*2, processMaxBackoff)

			p.mu.Lock()
			config := p.config
			p.mu.Unlock()
			next, caps, err := spawnAndRegister(p.ctx, p.spec, config)
			if err != nil {
				if p.ctx.Err() != nil {
					return
				}
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
			if caps.RequestHook != p.caps.RequestHook || !slices.Equal(caps.Events, p.caps.Events) {
				p.spec.Logf("[plugin %s] restarted process reported capabilities %+v; keeping %+v until the plugin is restarted by the host",
					p.spec.ID, caps, p.caps)
			}
			p.client = next
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
		// Wait until the process is gone: callers may remove the package
		// right away (upgrade rollback), which fails while it still runs.
		client.kill()
		client.waitExit(processExitWait)
		return nil, pluginsdk.Capabilities{}, fmt.Errorf("register: %w", err)
	}
	if result.SchemaVersion != 0 && result.SchemaVersion != pluginsdk.SchemaVersion {
		client.shutdown(ctx)
		return nil, pluginsdk.Capabilities{}, fmt.Errorf("register: unsupported schema_version %d", result.SchemaVersion)
	}
	return client, effectiveCapabilities(spec.Declared, result.Capabilities), nil
}

// resolveCommand maps argv[0] from the manifest to an executable path.
// Package-relative paths are made absolute: exec.Cmd resolves a relative
// Path against Cmd.Dir, which is the package directory itself.
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
		abs, err := filepath.Abs(full)
		if err != nil {
			return "", fmt.Errorf("resolve command %q: %w", name, err)
		}
		return abs, nil
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

	// The pipes are created here rather than with cmd.*Pipe so that exit
	// detection only depends on the process itself: descendants that
	// inherited stdout/stderr cannot delay it.
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	pipe := func() (r, w *os.File, err error) {
		r, w, err = os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	stdinR, stdinW, err := pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start %s: %w", filepath.Base(path), err)
	}
	// The child holds its own copies of these ends.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	c := &rpcClient{
		id:         spec.ID,
		cmd:        cmd,
		stdin:      stdinW,
		writeCh:    make(chan []byte, processWriteQueue),
		writerDone: make(chan struct{}),
		pending:    make(map[int64]chan rpcResult),
		exited:     make(chan struct{}),
		startedAt:  time.Now(),
		logf:       spec.Logf,
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		c.readLoop(stdoutR)
	}()
	go func() {
		defer readers.Done()
		c.logStderr(stderrR)
	}()
	go c.writeLoop(stdinW)
	go func() {
		err := cmd.Wait()
		c.setExitErr(err)
		drained := make(chan struct{})
		go func() {
			readers.Wait()
			close(drained)
		}()
		// Let the readers consume the final output, but do not wait for
		// descendants that still hold the pipes open.
		timer := time.NewTimer(processDrainTimeout)
		select {
		case <-drained:
		case <-timer.C:
		}
		timer.Stop()
		c.failPending()
		close(c.exited)
		// Unblocks the readers, and a writer stuck on a pipe that a
		// descendant holds open.
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stderrR.Close()
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
	stdin     *os.File
	startedAt time.Time
	logf      func(format string, args ...any)

	// writeCh feeds the single stdin writer, so a plugin that stops reading
	// stdin blocks no caller beyond its context.
	writeCh    chan []byte
	writerDone chan struct{}
	nextID     atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResult // nil once the connection is gone
	readErr error
	exitErr error
	exited  chan struct{}

	shutdownOnce sync.Once
}

func (c *rpcClient) call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}
	id := c.nextID.Add(1)
	line, err := json.Marshal(pluginsdk.Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(strconv.FormatInt(id, 10)),
		Method:  method,
		Params:  raw,
	})
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	ch := make(chan rpcResult, 1)

	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return errProcessExited
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(ctx, append(line, '\n')); err != nil {
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
	}
}

func (c *rpcClient) forget(id int64) {
	c.mu.Lock()
	if c.pending != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// failPending fails every outstanding call; later calls fail immediately.
func (c *rpcClient) failPending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcResult{err: errProcessExited}
	}
}

// send queues one protocol line for the writer.
func (c *rpcClient) send(ctx context.Context, line []byte) error {
	select {
	case <-c.writerDone:
		return errProcessExited
	default:
	}
	select {
	case c.writeCh <- line:
		return nil
	case <-c.writerDone:
		return errProcessExited
	case <-ctx.Done():
		return ctx.Err()
	}
}

// trySend queues a line without blocking. The read loop uses it: it must
// never wait for the writer.
func (c *rpcClient) trySend(line []byte) {
	select {
	case c.writeCh <- line:
	default:
	}
}

func (c *rpcClient) writeLoop(w io.Writer) {
	defer close(c.writerDone)
	for {
		select {
		case line := <-c.writeCh:
			if _, err := w.Write(line); err != nil {
				if !errors.Is(err, os.ErrClosed) {
					// The plugin closed stdin: the connection is unusable.
					c.logf("[plugin %s] write to stdin failed: %v", c.id, err)
					c.kill()
				}
				return
			}
		case <-c.exited:
			return
		}
	}
}

func (c *rpcClient) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readProtocolLine(reader)
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				c.mu.Lock()
				c.readErr = err
				c.mu.Unlock()
				// Unrecoverable framing error: stop the process.
				c.kill()
			}
			break
		}
	}
	c.failPending()
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
			reply, err := json.Marshal(pluginsdk.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &pluginsdk.RPCError{Code: pluginsdk.CodeMethodNotFound, Message: "method not found: " + msg.Method},
			})
			if err == nil {
				c.trySend(append(reply, '\n'))
			}
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
		ch <- rpcResult{err: &rpcError{Code: msg.Error.Code, Message: msg.Error.Message}}
		return
	}
	ch <- rpcResult{result: msg.Result}
}

// rpcError is an error response from the plugin. It means the plugin handled
// the call and refused it, unlike transport errors and timeouts, after which
// the outcome is unknown.
type rpcError struct {
	Code    int
	Message string
}

func (e *rpcError) Error() string { return "plugin error: " + e.Message }

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

// logStderr copies the plugin's stderr to the host log line by line. Long
// lines are truncated instead of ending the copy.
func (c *rpcClient) logStderr(r io.Reader) {
	reader := bufio.NewReader(r)
	var line []byte
	truncated := false
	flush := func() {
		if len(line) > 0 {
			suffix := ""
			if truncated {
				suffix = " ...(truncated)"
			}
			c.logf("[plugin %s] %s%s", c.id, string(line), suffix)
		}
		line, truncated = line[:0], false
	}
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			flush()
			break
		}
		n := min(len(chunk), processMaxLogLine-len(line))
		line = append(line, chunk[:n]...)
		truncated = truncated || n < len(chunk)
		if !isPrefix {
			flush()
		}
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

// waitExit waits up to timeout for the process to be reaped.
func (c *rpcClient) waitExit(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.exited:
		return true
	case <-timer.C:
		c.logf("[plugin %s] process did not exit within %s", c.id, timeout)
		return false
	}
}

// shutdown asks the process to exit, then closes stdin and finally kills it
// if it does not exit in time. It waits (bounded) until the process is gone.
func (c *rpcClient) shutdown(ctx context.Context) {
	c.shutdownOnce.Do(func() {
		sdCtx, cancel := context.WithTimeout(ctx, processShutdownTimeout)
		_ = c.call(sdCtx, pluginsdk.MethodShutdown, struct{}{}, nil)
		cancel()
		_ = c.stdin.Close()
		timer := time.NewTimer(processShutdownTimeout)
		defer timer.Stop()
		select {
		case <-c.exited:
			return
		case <-timer.C:
		}
		c.kill()
	})
	c.waitExit(processExitWait)
}
