package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// The test binary doubles as an external plugin: when the host starts it with
// GO_WANT_HELPER_PLUGIN=1 (set through the manifest runtime env) it serves the
// plugin protocol on stdin/stdout instead of running tests. HELPER_MODE picks
// the behavior:
//
//	reject        reject every request with a JSON echo (default)
//	echo_env      like reject, and include environment details
//	crash_once    exit without replying to the first request ever made
//	slow          sleep 400ms before answering requests for account "slow"
//	bad_register  fail plugin.register (Configure always errors)
//	stall         answer plugin.register, then never read stdin again
//	orphan        on the first request ever made, start a child that inherits
//	              stdout and stderr, then exit without replying
//	sleep         sleep for a while (the orphaned child)
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER_PLUGIN") == "1" {
		tmRunHelperPlugin()
		return
	}
	os.Exit(m.Run())
}

const tmProcID = "tm.proc"

// tmEcho is the JSON body the helper returns in its reject message.
type tmEcho struct {
	PID        int                `json:"pid"`
	Msg        string             `json:"msg"`
	ProxyType  string             `json:"proxy_type"`
	ClientIP   string             `json:"client_ip"`
	Platform   string             `json:"platform"`
	Account    string             `json:"account"`
	TargetHost string             `json:"target_host"`
	Method     string             `json:"method"`
	URL        string             `json:"url"`
	UA         string             `json:"ua"`
	Env        map[string]*string `json:"env,omitempty"`
	ResinKeys  []string           `json:"resin_keys,omitempty"`
	Cwd        string             `json:"cwd,omitempty"`
	Reg        *tmRegEcho         `json:"reg,omitempty"`
}

type tmRegEcho struct {
	SchemaVersion int    `json:"schema_version"`
	ResinVersion  string `json:"resin_version"`
	PluginID      string `json:"plugin_id"`
	DataDir       string `json:"data_dir"`
}

var tmEchoEnvKeys = []string{
	"RESIN_ADMIN_TOKEN", "RESIN_FROM_MANIFEST", "RESIN_PLUGIN_ID", "RESIN_PLUGIN_DIR",
	"RESIN_PLUGIN_DATA_DIR", "TM_INHERITED", "TM_EXTRA",
}

// --- plugin side (runs in the child process) ---

type tmHelperPlugin struct {
	mode string
	mu   sync.Mutex
	msg  string
	reg  pluginsdk.RegisterParams
}

func tmRunHelperPlugin() {
	mode := os.Getenv("HELPER_MODE")
	switch mode {
	case "stall":
		tmServeStalled()
	case "sleep":
		// Bounded, so a leaked child never outlives the test run for long.
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	p := &tmHelperPlugin{mode: mode}
	if err := pluginsdk.Serve(p); err != nil {
		fmt.Fprintln(os.Stderr, "helper plugin:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// tmServeStalled answers plugin.register and then stops reading stdin, like
// a plugin wedged in a long synchronous call.
func tmServeStalled() {
	line, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil {
		os.Exit(2)
	}
	var msg pluginsdk.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		os.Exit(2)
	}
	result, _ := json.Marshal(pluginsdk.RegisterResult{SchemaVersion: pluginsdk.SchemaVersion})
	reply, _ := json.Marshal(pluginsdk.Message{JSONRPC: "2.0", ID: msg.ID, Result: result})
	_, _ = os.Stdout.Write(append(reply, '\n'))
	time.Sleep(20 * time.Second)
	os.Exit(0)
}

// orphanAndExit starts a copy of the helper that inherits stdout and stderr
// (like a plugin that spawns a daemon), records its pid and exits.
func (p *tmHelperPlugin) orphanAndExit() {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(4)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "HELPER_MODE=sleep")
	cmd.Dir = os.TempDir() // keeps the package directory removable
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(4)
	}
	_ = os.WriteFile(p.dataFile("orphan.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	os.Exit(3)
}

func (p *tmHelperPlugin) Init(_ context.Context, params pluginsdk.RegisterParams) error {
	p.mu.Lock()
	p.reg = params
	p.mu.Unlock()
	return nil
}

func (p *tmHelperPlugin) Configure(_ context.Context, raw json.RawMessage) error {
	if p.mode == "bad_register" {
		return errors.New("helper refuses to register")
	}
	var cfg struct {
		Msg  string `json:"msg"`
		Fail bool   `json:"fail"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.Fail {
		return errors.New("helper rejects config")
	}
	p.mu.Lock()
	p.msg = cfg.Msg
	p.mu.Unlock()
	return nil
}

func (p *tmHelperPlugin) dataFile(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return filepath.Join(p.reg.DataDir, name)
}

func (p *tmHelperPlugin) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	switch {
	case p.mode == "crash_once":
		marker := p.dataFile("crashed")
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o644)
			os.Exit(3)
		}
	case p.mode == "slow" && req.Account == "slow":
		time.Sleep(400 * time.Millisecond)
	case p.mode == "orphan":
		if _, err := os.Stat(p.dataFile("orphan.pid")); err != nil {
			p.orphanAndExit()
		}
	}
	p.mu.Lock()
	msg, reg := p.msg, p.reg
	p.mu.Unlock()

	switch req.Account {
	case "pass":
		account, platform := "rewritten", "plat-"+msg
		return &pluginsdk.RequestDecision{
			Platform:      &platform,
			Account:       &account,
			SetHeaders:    map[string]string{"X-Proc": msg},
			RemoveHeaders: []string{"X-Drop"},
		}, nil
	case "error":
		return nil, errors.New("helper inspect failure")
	}

	echo := tmEcho{
		PID: os.Getpid(), Msg: msg, ProxyType: req.ProxyType, ClientIP: req.ClientIP,
		Platform: req.Platform, Account: req.Account, TargetHost: req.TargetHost,
		Method: req.Method, URL: req.URL,
	}
	if ua := req.Headers["User-Agent"]; len(ua) > 0 {
		echo.UA = ua[0]
	}
	if p.mode == "echo_env" {
		echo.Env = make(map[string]*string)
		for _, k := range tmEchoEnvKeys {
			if v, ok := os.LookupEnv(k); ok {
				echo.Env[k] = &v
			} else {
				echo.Env[k] = nil
			}
		}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(strings.ToUpper(k), "RESIN_") {
				echo.ResinKeys = append(echo.ResinKeys, strings.ToUpper(k))
			}
		}
		sort.Strings(echo.ResinKeys)
		echo.Cwd, _ = os.Getwd()
		echo.Reg = &tmRegEcho{SchemaVersion: reg.SchemaVersion, ResinVersion: reg.ResinVersion, PluginID: reg.PluginID, DataDir: reg.DataDir}
	}
	data, err := json.Marshal(echo)
	if err != nil {
		return nil, err
	}
	return pluginsdk.Reject(418, string(data)), nil
}

func (p *tmHelperPlugin) HandleEvents(_ context.Context, events []pluginsdk.Event) error {
	f, err := os.OpenFile(p.dataFile("events.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func (p *tmHelperPlugin) Shutdown(context.Context) error {
	f, err := os.OpenFile(p.dataFile("shutdown.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%d\n", os.Getpid())
	return err
}

// --- host side ---

type tmProc struct {
	root    string
	pkgDir  string
	dataDir string
	m       *Manager
}

// tmHelperManifest returns a manifest that runs this test binary as the plugin.
func tmHelperManifest(t *testing.T, mode string) pluginsdk.Manifest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !filepath.IsAbs(exe) {
		if exe, err = filepath.Abs(exe); err != nil {
			t.Fatal(err)
		}
	}
	return pluginsdk.Manifest{
		SchemaVersion: pluginsdk.SchemaVersion,
		ID:            tmProcID,
		Name:          "TM process helper",
		Version:       "1.0.0",
		Capabilities:  pluginsdk.Capabilities{RequestHook: true},
		ConfigFields: []pluginsdk.ConfigField{
			{Name: "msg", Type: pluginsdk.FieldString, Default: json.RawMessage(`"default"`)},
			{Name: "fail", Type: pluginsdk.FieldBoolean},
		},
		Runtimes: map[string]pluginsdk.RuntimeSpec{
			pluginsdk.RuntimeAny: {
				Command: []string{exe, "-test.run=^$"},
				Env:     map[string]string{"GO_WANT_HELPER_PLUGIN": "1", "HELPER_MODE": mode},
			},
		},
	}
}

// tmPackageManager installs mf as package tm.proc in a fresh plugin dir and
// starts a manager with external plugins enabled.
func tmPackageManager(t *testing.T, mf pluginsdk.Manifest) *tmProc {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, tmProcID)
	tmWriteFile(t, filepath.Join(pkgDir, pluginsdk.ManifestFileName), tmManifestJSON(t, mf))
	m := tmStartManager(t, ManagerConfig{PluginDir: root, ExternalEnabled: true, ResinVersion: "1.2.3"})
	return &tmProc{root: root, pkgDir: pkgDir, dataDir: filepath.Join(root, ".data", tmProcID), m: m}
}

func tmProcSetup(t *testing.T, mode string, mutate func(*pluginsdk.Manifest)) *tmProc {
	t.Helper()
	if testing.Short() {
		t.Skip("spawns plugin processes; skipped in -short mode")
	}
	mf := tmHelperManifest(t, mode)
	if mutate != nil {
		mutate(&mf)
	}
	return tmPackageManager(t, mf)
}

func (p *tmProc) inspect(account string) proxy.RequestHookResult {
	req := tmRequest()
	req.Account = account
	return p.m.InspectRequest(context.Background(), req)
}

func tmDecodeEcho(t *testing.T, res proxy.RequestHookResult) tmEcho {
	t.Helper()
	if res.Reject == nil {
		t.Fatalf("expected a reject echo from the helper, got %+v", res)
	}
	if res.Reject.HTTPCode != 418 || res.PluginID != tmProcID {
		t.Fatalf("reject = %+v plugin=%q", res.Reject, res.PluginID)
	}
	var echo tmEcho
	if err := json.Unmarshal([]byte(res.Reject.Message), &echo); err != nil {
		t.Fatalf("decode echo %q: %v", res.Reject.Message, err)
	}
	return echo
}

func tmReadLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func tmSamePath(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

func TestProcessRequestRoundTrip(t *testing.T) {
	p := tmProcSetup(t, "reject", nil)
	info := tmGet(t, p.m, tmProcID)
	if info.Source != SourcePackage || info.Enabled || info.Status != StatusStopped || info.LastError != "" {
		t.Fatalf("package before enable = %+v", info)
	}
	if string(info.Config) != `{"msg":"default"}` {
		t.Fatalf("default config = %s", info.Config)
	}

	info = tmEnable(t, p.m, tmProcID, Update{})
	if !info.Capabilities.RequestHook || len(info.Capabilities.Events) != 0 {
		t.Fatalf("effective capabilities = %+v (manifest declares no events)", info.Capabilities)
	}
	if !p.m.Active() {
		t.Fatal("Active() = false with a running request plugin")
	}

	echo := tmDecodeEcho(t, p.inspect("acct"))
	want := tmEcho{
		PID: echo.PID, Msg: "default", ProxyType: pluginsdk.ProxyTypeReverse, ClientIP: "10.0.0.1",
		Platform: "plat", Account: "acct", TargetHost: "example.com", Method: "GET",
		URL: "https://example.com/x", UA: "test",
	}
	if !reflect.DeepEqual(echo, want) {
		t.Fatalf("echo = %+v, want %+v", echo, want)
	}
	if echo.PID == 0 || echo.PID == os.Getpid() {
		t.Fatalf("plugin pid = %d (host %d)", echo.PID, os.Getpid())
	}

	req := tmRequest()
	req.Account = "pass"
	req.Headers["X-Drop"] = []string{"1"}
	res := p.m.InspectRequest(context.Background(), req)
	if res.Reject != nil {
		t.Fatalf("continue decision rejected: %+v", res.Reject)
	}
	if res.Platform != "plat-default" || res.Account != "rewritten" {
		t.Fatalf("identity = %q/%q", res.Platform, res.Account)
	}
	wantOps := []proxy.HeaderOp{{Name: "X-Drop", Remove: true}, {Name: "X-Proc", Value: "default"}}
	if !reflect.DeepEqual(res.HeaderOps, wantOps) {
		t.Fatalf("header ops = %+v, want %+v", res.HeaderOps, wantOps)
	}

	if res := p.inspect("error"); res.Reject != nil || res.PluginID != "" {
		t.Fatalf("plugin error with fail-open = %+v", res)
	}
	tmUpdate(t, p.m, tmProcID, Update{FailClosed: tmPtr(true)})
	if res := p.inspect("error"); res.Reject != proxy.ErrPluginUnavailable || res.PluginID != tmProcID {
		t.Fatalf("plugin error with fail-closed = %+v", res)
	}

	st := tmGet(t, p.m, tmProcID).Stats
	if st.Requests != 4 || st.Rejects != 1 || st.Errors != 2 || st.Timeouts != 0 || st.Restarts != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if got := tmGet(t, p.m, tmProcID); got.Status != StatusRunning {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestProcessEnvIsolation(t *testing.T) {
	t.Setenv("RESIN_ADMIN_TOKEN", "secret")
	t.Setenv("RESIN_PLUGIN_ID", "spoofed")
	t.Setenv("TM_INHERITED", "yes")
	p := tmProcSetup(t, "echo_env", func(mf *pluginsdk.Manifest) {
		env := mf.Runtimes[pluginsdk.RuntimeAny].Env
		env["RESIN_FROM_MANIFEST"] = "x"
		env["TM_EXTRA"] = "extra"
	})
	tmEnable(t, p.m, tmProcID, Update{})
	echo := tmDecodeEcho(t, p.inspect("acct"))

	str := func(p *string) string {
		if p == nil {
			return "<unset>"
		}
		return *p
	}
	if v := echo.Env["RESIN_ADMIN_TOKEN"]; v != nil {
		t.Fatalf("RESIN_ADMIN_TOKEN leaked to the plugin: %q", *v)
	}
	if v := echo.Env["RESIN_FROM_MANIFEST"]; v != nil {
		t.Fatalf("manifest may not set RESIN_* variables, got %q", *v)
	}
	if got := str(echo.Env["RESIN_PLUGIN_ID"]); got != tmProcID {
		t.Fatalf("RESIN_PLUGIN_ID = %q", got)
	}
	if got := str(echo.Env["RESIN_PLUGIN_DIR"]); !tmSamePath(got, p.pkgDir) {
		t.Fatalf("RESIN_PLUGIN_DIR = %q, want %q", got, p.pkgDir)
	}
	if got := str(echo.Env["RESIN_PLUGIN_DATA_DIR"]); !tmSamePath(got, p.dataDir) {
		t.Fatalf("RESIN_PLUGIN_DATA_DIR = %q, want existing %q", got, p.dataDir)
	}
	if got := str(echo.Env["TM_INHERITED"]); got != "yes" {
		t.Fatalf("regular env not inherited: TM_INHERITED = %q", got)
	}
	if got := str(echo.Env["TM_EXTRA"]); got != "extra" {
		t.Fatalf("manifest env not applied: TM_EXTRA = %q", got)
	}
	if want := []string{"RESIN_PLUGIN_DATA_DIR", "RESIN_PLUGIN_DIR", "RESIN_PLUGIN_ID"}; !reflect.DeepEqual(echo.ResinKeys, want) {
		t.Fatalf("RESIN_* keys visible to plugin = %v, want %v", echo.ResinKeys, want)
	}
	if !tmSamePath(echo.Cwd, p.pkgDir) {
		t.Fatalf("plugin cwd = %q, want package dir %q", echo.Cwd, p.pkgDir)
	}
	if echo.Reg == nil || echo.Reg.SchemaVersion != pluginsdk.SchemaVersion || echo.Reg.ResinVersion != "1.2.3" ||
		echo.Reg.PluginID != tmProcID || !tmSamePath(echo.Reg.DataDir, p.dataDir) {
		t.Fatalf("register params = %+v", echo.Reg)
	}
}

func TestProcessHotConfigure(t *testing.T) {
	p := tmProcSetup(t, "reject", nil)
	tmEnable(t, p.m, tmProcID, Update{Config: json.RawMessage(`{"msg":"v1"}`)})
	e1 := tmDecodeEcho(t, p.inspect("acct"))
	if e1.Msg != "v1" {
		t.Fatalf("initial config not applied at register: %+v", e1)
	}

	info := tmUpdate(t, p.m, tmProcID, Update{Config: json.RawMessage(`{"msg":"v2"}`)})
	if string(info.Config) != `{"msg":"v2"}` || info.Status != StatusRunning {
		t.Fatalf("after hot configure: %+v", info)
	}
	e2 := tmDecodeEcho(t, p.inspect("acct"))
	if e2.Msg != "v2" || e2.PID != e1.PID {
		t.Fatalf("hot configure: echo %+v (pid before %d); want msg v2 without restart", e2, e1.PID)
	}

	_, err := p.m.Update(context.Background(), tmProcID, Update{Config: json.RawMessage(`{"msg":"v3","fail":true}`)})
	if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "helper rejects config") {
		t.Fatalf("rejected hot configure err = %v, want ErrInvalidArgument from plugin", err)
	}
	e3 := tmDecodeEcho(t, p.inspect("acct"))
	if e3.Msg != "v2" || e3.PID != e1.PID {
		t.Fatalf("after rejected configure: echo %+v; want previous config v2 kept", e3)
	}
	if got := tmGet(t, p.m, tmProcID); string(got.Config) != `{"msg":"v2"}` || got.Status != StatusRunning {
		t.Fatalf("state after rejected configure: %+v", got)
	}

	// A config stored while disabled is used at the next register.
	tmUpdate(t, p.m, tmProcID, Update{Enabled: tmPtr(false)})
	tmUpdate(t, p.m, tmProcID, Update{Config: json.RawMessage(`{"msg":"v4"}`)})
	tmEnable(t, p.m, tmProcID, Update{})
	e4 := tmDecodeEcho(t, p.inspect("acct"))
	if e4.Msg != "v4" || e4.PID == e1.PID {
		t.Fatalf("after re-enable: echo %+v; want msg v4 in a new process", e4)
	}
	if st := tmGet(t, p.m, tmProcID).Stats; st.Restarts != 0 {
		t.Fatalf("restarts = %d; hot configure and re-enable are not crash restarts", st.Restarts)
	}
}

func TestProcessRestartAfterExit(t *testing.T) {
	p := tmProcSetup(t, "crash_once", nil)
	tmEnable(t, p.m, tmProcID, Update{Config: json.RawMessage(`{"msg":"v1"}`)})
	tmUpdate(t, p.m, tmProcID, Update{Config: json.RawMessage(`{"msg":"v2"}`)})

	// The first request makes the process exit without answering.
	if res := p.inspect("acct"); res.Reject != nil {
		t.Fatalf("crashing plugin with fail-open = %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(p.dataDir, "crashed"))
	if err != nil {
		t.Fatalf("crash marker: %v", err)
	}
	crashedPID, _ := strconv.Atoi(string(raw))

	// Restart backoff starts at 1s: the plugin reports "starting" meanwhile.
	tmWaitFor(t, 3*time.Second, "status starting after the crash", func() bool {
		return tmGet(t, p.m, tmProcID).Status == StatusStarting
	})
	if got := tmGet(t, p.m, tmProcID); !strings.Contains(got.LastError, "process exited") {
		t.Fatalf("last_error while restarting = %q", got.LastError)
	}
	tmWaitFor(t, 5*time.Second, "automatic restart", func() bool {
		got := tmGet(t, p.m, tmProcID)
		return got.Stats.Restarts == 1 && got.Status == StatusRunning
	})

	got := tmGet(t, p.m, tmProcID)
	if got.LastError != "" || !got.Enabled {
		t.Fatalf("after restart: %+v", got)
	}
	echo := tmDecodeEcho(t, p.inspect("acct"))
	if echo.Msg != "v2" {
		t.Fatalf("restarted process got config %q, want latest hot config v2", echo.Msg)
	}
	if echo.PID == crashedPID {
		t.Fatalf("restarted process has the crashed pid %d", echo.PID)
	}
	st := tmGet(t, p.m, tmProcID).Stats
	if st.Errors < 1 || st.Rejects != 1 || st.Restarts != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestProcessStartFailures(t *testing.T) {
	cases := []struct {
		name    string
		process bool
		mutate  func(*pluginsdk.Manifest)
		wantErr string
	}{
		{name: "register error", process: true, wantErr: "helper refuses to register"},
		{name: "missing command", mutate: func(mf *pluginsdk.Manifest) {
			mf.Runtimes = map[string]pluginsdk.RuntimeSpec{pluginsdk.RuntimeAny: {Command: []string{"bin/missing"}}}
		}, wantErr: "not found in plugin package"},
		{name: "command escapes package", mutate: func(mf *pluginsdk.Manifest) {
			mf.Runtimes = map[string]pluginsdk.RuntimeSpec{pluginsdk.RuntimeAny: {Command: []string{"../outside/plugin"}}}
		}, wantErr: "escapes the plugin directory"},
		{name: "command not in PATH", mutate: func(mf *pluginsdk.Manifest) {
			mf.Runtimes = map[string]pluginsdk.RuntimeSpec{pluginsdk.RuntimeAny: {Command: []string{"tm-no-such-plugin-binary"}}}
		}, wantErr: "not found in PATH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p *tmProc
			if tc.process {
				p = tmProcSetup(t, "bad_register", tc.mutate)
			} else {
				mf := tmHelperManifest(t, "reject")
				tc.mutate(&mf)
				p = tmPackageManager(t, mf)
			}
			_, err := p.m.Update(context.Background(), tmProcID, Update{Enabled: tmPtr(true)})
			if !errors.Is(err, ErrStartFailed) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("enable err = %v, want ErrStartFailed containing %q", err, tc.wantErr)
			}
			got := tmGet(t, p.m, tmProcID)
			if got.Enabled || got.Status != StatusError || !strings.Contains(got.LastError, tc.wantErr) {
				t.Fatalf("state after failed start: %+v", got)
			}
			if p.m.Active() {
				t.Fatal("Active() after failed start")
			}
			if res := p.inspect("acct"); res.Reject != nil {
				t.Fatalf("failed plugin in chain: %+v", res)
			}
		})
	}
}

func TestProcessStopShutsDownPromptly(t *testing.T) {
	p := tmProcSetup(t, "reject", nil)
	shutdownLog := filepath.Join(p.dataDir, "shutdown.log")

	tmEnable(t, p.m, tmProcID, Update{})
	e1 := tmDecodeEcho(t, p.inspect("acct"))
	start := time.Now()
	info := tmUpdate(t, p.m, tmProcID, Update{Enabled: tmPtr(false)})
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("disable took %v; graceful shutdown should not hit the 3s timeout", elapsed)
	}
	if info.Status != StatusStopped || p.m.Active() {
		t.Fatalf("after disable: %+v", info)
	}
	if lines := tmReadLines(t, shutdownLog); !reflect.DeepEqual(lines, []string{strconv.Itoa(e1.PID)}) {
		t.Fatalf("shutdown log after disable = %v, want [%d]", lines, e1.PID)
	}

	tmEnable(t, p.m, tmProcID, Update{})
	e2 := tmDecodeEcho(t, p.inspect("acct"))
	start = time.Now()
	p.m.Stop(context.Background())
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("Stop took %v", elapsed)
	}
	if lines := tmReadLines(t, shutdownLog); !reflect.DeepEqual(lines, []string{strconv.Itoa(e1.PID), strconv.Itoa(e2.PID)}) {
		t.Fatalf("shutdown log after Stop = %v", lines)
	}
	if got := tmGet(t, p.m, tmProcID); got.Status != StatusStopped || got.Stats.Restarts != 0 {
		t.Fatalf("after Stop: %+v", got)
	}
	if res := p.inspect("acct"); res.Reject != nil {
		t.Fatalf("stopped manager still inspects: %+v", res)
	}
	if _, err := p.m.Update(context.Background(), tmProcID, Update{Enabled: tmPtr(true)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update after Stop err = %v", err)
	}
}

func TestProcessEventsDelivered(t *testing.T) {
	p := tmProcSetup(t, "reject", func(mf *pluginsdk.Manifest) {
		mf.Capabilities = pluginsdk.Capabilities{Events: []string{"lease.*"}}
	})
	info := tmEnable(t, p.m, tmProcID, Update{})
	wantEvents := []string{pluginsdk.EventLeaseCreated, pluginsdk.EventLeaseReplaced, pluginsdk.EventLeaseRemoved, pluginsdk.EventLeaseExpired}
	if info.Capabilities.RequestHook || !reflect.DeepEqual(info.Capabilities.Events, wantEvents) {
		t.Fatalf("effective capabilities = %+v", info.Capabilities)
	}
	if p.m.Active() {
		t.Fatal("Active() = true for an events-only plugin")
	}

	p.m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p1", Account: "a1"})
	p.m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseTouch, PlatformID: "p1", Account: "a1"})
	p.m.ObserveRequest(proxy.RequestLogEntry{PlatformID: "p1", Account: "a1"}) // not subscribed
	p.m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseRemove, PlatformID: "p1", Account: "a1"})

	// Disabling flushes the queue over RPC before the process is shut down.
	tmUpdate(t, p.m, tmProcID, Update{Enabled: tmPtr(false)})

	lines := tmReadLines(t, filepath.Join(p.dataDir, "events.log"))
	var types []string
	for _, line := range lines {
		var ev pluginsdk.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event line %q: %v", line, err)
		}
		var data pluginsdk.LeaseEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil || data.PlatformID != "p1" || data.Account != "a1" {
			t.Fatalf("event data %s (err %v)", ev.Data, err)
		}
		types = append(types, ev.Type)
	}
	if want := []string{pluginsdk.EventLeaseCreated, pluginsdk.EventLeaseRemoved}; !reflect.DeepEqual(types, want) {
		t.Fatalf("plugin received %v, want %v", types, want)
	}
	if st := tmGet(t, p.m, tmProcID).Stats; st.EventsDelivered != 2 || st.EventErrors != 0 || st.EventsDropped != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestProcessInspectTimeout(t *testing.T) {
	p := tmProcSetup(t, "slow", nil)
	tmEnable(t, p.m, tmProcID, Update{TimeoutMs: tmPtr(50)})

	start := time.Now()
	if res := p.inspect("slow"); res.Reject != nil {
		t.Fatalf("timed out call applied: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 350*time.Millisecond {
		t.Fatalf("inspect took %v with a 50ms timeout", elapsed)
	}

	tmUpdate(t, p.m, tmProcID, Update{FailClosed: tmPtr(true)})
	if res := p.inspect("slow"); res.Reject != proxy.ErrPluginUnavailable {
		t.Fatalf("fail-closed timeout = %+v", res)
	}

	// Late answers are discarded and the connection keeps working.
	tmUpdate(t, p.m, tmProcID, Update{TimeoutMs: tmPtr(5000), FailClosed: tmPtr(false)})
	echo := tmDecodeEcho(t, p.inspect("acct"))
	if echo.Account != "acct" {
		t.Fatalf("echo = %+v", echo)
	}
	st := tmGet(t, p.m, tmProcID).Stats
	if st.Requests != 3 || st.Timeouts != 2 || st.Errors != 0 || st.Rejects != 1 || st.Restarts != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A plugin that stops reading stdin must not block callers beyond their
// context, even once the pipe and the write queue are full.
func TestProcessStalledStdinDoesNotBlockCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns plugin processes; skipped in -short mode")
	}
	mf := tmHelperManifest(t, "stall")
	dir := t.TempDir()
	spec := processSpec{
		ID:       tmProcID,
		Dir:      dir,
		DataDir:  filepath.Join(dir, "data"),
		Runtime:  mf.Runtimes[pluginsdk.RuntimeAny],
		Declared: mf.Capabilities,
		Logf:     tmLogf(t),
	}
	p, err := startProcess(context.Background(), spec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("startProcess: %v", err)
	}
	t.Cleanup(func() {
		if c, err := p.current(); err == nil {
			c.kill()
		}
		_ = p.Close(context.Background())
	})

	req := tmRequest()
	req.URL = "https://example.com/" + strings.Repeat("x", 16<<10)
	const calls = processWriteQueue + 64
	errs := make(chan error, calls)
	start := time.Now()
	for range calls {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := p.Inspect(ctx, req)
			errs <- err
		}()
	}
	deadline := time.After(5 * time.Second)
	for i := range calls {
		select {
		case err := <-errs:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("call err = %v, want context.DeadlineExceeded", err)
			}
		case <-deadline:
			t.Fatalf("%d of %d calls still blocked after %v", calls-i, calls, time.Since(start))
		}
	}
}

// A descendant holding the inherited stdout/stderr must not hide the exit of
// the plugin process itself.
func TestProcessExitWithInheritedPipes(t *testing.T) {
	p := tmProcSetup(t, "orphan", nil)
	tmEnable(t, p.m, tmProcID, Update{TimeoutMs: tmPtr(5000)})
	pidFile := filepath.Join(p.dataDir, "orphan.pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			return
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
			_ = proc.Release()
		}
	})

	start := time.Now()
	if res := p.inspect("acct"); res.Reject != nil {
		t.Fatalf("exited plugin with fail-open = %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("pending call failed after %v, want soon after the process exited", elapsed)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("orphan was not started: %v", err)
	}
	tmWaitFor(t, 5*time.Second, "automatic restart while the orphan holds the pipes", func() bool {
		got := tmGet(t, p.m, tmProcID)
		return got.Stats.Restarts == 1 && got.Status == StatusRunning
	})
	if echo := tmDecodeEcho(t, p.inspect("acct")); echo.Msg != "default" {
		t.Fatalf("restarted process echo = %+v", echo)
	}
}

// --- resolveCommand / pluginEnv units ---

func TestResolveCommand(t *testing.T) {
	dir := t.TempDir()
	tmWriteFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n")
	tmWriteFile(t, filepath.Join(dir, "bin", "tool"), "x")
	if runtime.GOOS == "windows" {
		tmWriteFile(t, filepath.Join(dir, "bin", "winexe.exe"), "x")
	}

	pathDir := t.TempDir()
	lookName := "tmlookuptool"
	lookFile := filepath.Join(pathDir, lookName)
	if runtime.GOOS == "windows" {
		lookFile += ".exe"
	}
	tmWriteFile(t, lookFile, "x")
	if err := os.Chmod(lookFile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	abs := filepath.Join(t.TempDir(), "does-not-need-to-exist")
	type tc struct {
		name, want, wantErr string
	}
	cases := []tc{
		{name: abs, want: abs},
		{name: "./run.sh", want: filepath.Join(dir, "run.sh")},
		{name: "bin/tool", want: filepath.Join(dir, "bin", "tool")},
		{name: "bin/./sub/../tool", want: filepath.Join(dir, "bin", "tool")},
		{name: "bin/missing", wantErr: "not found in plugin package"},
		{name: "./missing", wantErr: "not found in plugin package"},
		{name: "../outside", wantErr: "escapes the plugin directory"},
		{name: "..", wantErr: "escapes the plugin directory"},
		{name: "bin/../../outside", wantErr: "escapes the plugin directory"},
		{name: "./bin/../../outside", wantErr: "escapes the plugin directory"},
		{name: lookName, want: lookFile},
		{name: "tm-no-such-binary-xyz", wantErr: "not found in PATH"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			tc{name: `bin\tool`, want: filepath.Join(dir, "bin", "tool")},
			tc{name: `..\outside`, wantErr: "escapes the plugin directory"},
			tc{name: "bin/winexe", want: filepath.Join(dir, "bin", "winexe.exe")},
		)
	}
	for _, c := range cases {
		got, err := resolveCommand(dir, c.name)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("resolveCommand(%q) = %q, %v; want error containing %q", c.name, got, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveCommand(%q) error: %v", c.name, err)
			continue
		}
		if !strings.EqualFold(filepath.Clean(got), filepath.Clean(c.want)) {
			t.Errorf("resolveCommand(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPluginEnv(t *testing.T) {
	t.Setenv("RESIN_ADMIN_TOKEN", "secret")
	t.Setenv("RESIN_PLUGIN_ID", "spoofed")
	t.Setenv("Resin_Mixed_Case", "mixed")
	t.Setenv("TM_KEEP", "keep")
	t.Setenv("TM_RESIN_SUFFIX", "not-a-prefix")
	spec := processSpec{
		ID:      "tm.env",
		Dir:     filepath.Join("pkg", "tm.env"),
		DataDir: filepath.Join("pkg", ".data", "tm.env"),
		Runtime: pluginsdk.RuntimeSpec{Env: map[string]string{
			"RESIN_OVERRIDE": "x",
			"resin_lower":    "y",
			"TM_MANIFEST":    "m",
		}},
	}
	env := pluginEnv(spec)

	values := make(map[string][]string)
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		values[k] = append(values[k], v)
		if strings.Contains(v, "secret") || strings.Contains(v, "spoofed") {
			t.Fatalf("parent RESIN_* value leaked: %q", kv)
		}
	}
	var resinKeys []string
	for k := range values {
		if strings.HasPrefix(strings.ToUpper(k), "RESIN_") {
			resinKeys = append(resinKeys, k)
		}
	}
	sort.Strings(resinKeys)
	if want := []string{"RESIN_PLUGIN_DATA_DIR", "RESIN_PLUGIN_DIR", "RESIN_PLUGIN_ID"}; !reflect.DeepEqual(resinKeys, want) {
		t.Fatalf("RESIN_* keys = %v, want %v", resinKeys, want)
	}
	want := map[string]string{
		"RESIN_PLUGIN_ID":       spec.ID,
		"RESIN_PLUGIN_DIR":      spec.Dir,
		"RESIN_PLUGIN_DATA_DIR": spec.DataDir,
		"TM_KEEP":               "keep",
		"TM_RESIN_SUFFIX":       "not-a-prefix",
		"TM_MANIFEST":           "m",
	}
	for k, v := range want {
		if got := values[k]; !reflect.DeepEqual(got, []string{v}) {
			t.Errorf("%s = %q, want [%q]", k, got, v)
		}
	}
	n := len(env)
	if n < 3 || env[n-3] != "RESIN_PLUGIN_ID="+spec.ID || env[n-2] != "RESIN_PLUGIN_DIR="+spec.Dir || env[n-1] != "RESIN_PLUGIN_DATA_DIR="+spec.DataDir {
		t.Fatalf("plugin variables must be appended last, got tail %q", env[max(0, n-3):])
	}
}
