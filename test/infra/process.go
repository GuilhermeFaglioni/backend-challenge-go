//go:build integration || multiinstance

package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BuildBinary compiles the application the way the multi-instance layer needs
// it: with the fault points (-tags faultinject), the race detector and coverage
// instrumentation, so the separate processes are checked for data races and
// their coverage can be merged with the other layers'. It returns the path.
func BuildBinary(ctx context.Context, outDir string) (string, error) {
	binary := filepath.Join(outDir, "api")
	// The output path is a directory the test run created, not external input.
	cmd := exec.CommandContext(ctx, "go", "build", //nolint:gosec // fixed arguments; only the output path varies
		"-race", "-cover", "-covermode=atomic", "-coverpkg=./internal/...,./cmd/...",
		"-tags", "faultinject",
		"-o", binary, "./cmd/api")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return binary, nil
}

// LogBuffer collects a process's output. The application writes JSON logs to
// stdout and plain text (configuration errors, fault points) to stderr.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *LogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String returns everything written so far.
func (l *LogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Messages returns the message of each log line, in order: the "msg" field of
// a JSON line, or the whole line when it is not JSON.
func (l *LogBuffer) Messages() []string {
	var msgs []string
	for _, line := range strings.Split(l.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg != "" {
			msgs = append(msgs, entry.Msg)
			continue
		}
		msgs = append(msgs, line)
	}
	return msgs
}

// Instance is one running application process: its own memory, its own
// connections, nothing shared with the test or with the other instances.
type Instance struct {
	Name string
	Addr string
	Logs *LogBuffer

	cfg        InstanceConfig
	binary     string
	generation int
	cmd        *exec.Cmd
	exited     chan struct{}
	mu         sync.Mutex
	status     *os.ProcessState
}

// InstanceConfig describes a process to start.
type InstanceConfig struct {
	Name string
	// LogName names the saved log file; it defaults to Name. Give it something
	// unique across tests, or two failing tests overwrite each other's file.
	LogName string
	// Addr is the HTTP listen address, e.g. 127.0.0.1:18081.
	Addr string
	// Env is the full environment of the process (the application reads its
	// configuration from it); nothing is inherited from the test.
	Env map[string]string
}

// started records every instance of the run, so their logs can still be saved
// when the run hangs and no test cleanup gets to run.
var started struct {
	mu        sync.Mutex
	instances []*Instance
}

// StartInstance launches the binary and returns once the process exists. It
// does not wait for readiness: use WaitReady. The process is tied to ctx: when
// ctx ends (the test is over) the process is killed, so none outlives its test.
func StartInstance(ctx context.Context, binary string, cfg InstanceConfig) (*Instance, error) {
	cmd := exec.CommandContext(ctx, binary) //nolint:gosec // the binary is the one this run compiled
	cmd.Dir = RepoRoot()
	env := make([]string, 0, len(cfg.Env)+1)
	for key, value := range cfg.Env {
		env = append(env, key+"="+value)
	}
	cmd.Env = env

	logs := &LogBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Name, err)
	}
	i := &Instance{Name: cfg.Name, Addr: cfg.Addr, Logs: logs, cfg: cfg, binary: binary, cmd: cmd, exited: make(chan struct{})}
	started.mu.Lock()
	started.instances = append(started.instances, i)
	started.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		i.mu.Lock()
		i.status = cmd.ProcessState
		i.mu.Unlock()
		close(i.exited)
	}()
	return i, nil
}

// Restart starts a new process with the same name, address and environment,
// after this one has exited. Its log is kept apart from this one's.
func (i *Instance) Restart(ctx context.Context) (*Instance, error) {
	if i.Running() {
		return nil, fmt.Errorf("%s is still running; Kill or Terminate it first", i.Name)
	}
	cfg := i.cfg
	cfg.LogName = fmt.Sprintf("%s-run%d", i.logName(), i.generation+2)
	next, err := StartInstance(ctx, i.binary, cfg)
	if err != nil {
		return nil, err
	}
	next.generation = i.generation + 1
	return next, nil
}

func (i *Instance) logName() string {
	if i.cfg.LogName != "" {
		return i.cfg.LogName
	}
	return i.Name
}

// Pid is the operating-system process id.
func (i *Instance) Pid() int { return i.cmd.Process.Pid }

// Running reports whether the process is still alive.
func (i *Instance) Running() bool {
	select {
	case <-i.exited:
		return false
	default:
		return true
	}
}

// Kill ends the process abruptly (SIGKILL), as a crash or an out-of-memory kill
// would: no shutdown hooks run and no coverage is flushed.
func (i *Instance) Kill() {
	if i.Running() {
		_ = i.cmd.Process.Kill()
	}
	<-i.exited
}

// Terminate asks the process to stop (SIGTERM, like an orchestrator) and waits
// for it. It returns the exit code.
func (i *Instance) Terminate(ctx context.Context) (int, error) {
	if i.Running() {
		if err := i.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return -1, err
		}
	}
	return i.WaitExit(ctx)
}

// WaitExit blocks until the process has exited and returns its exit code (-1
// when it was killed by a signal).
func (i *Instance) WaitExit(ctx context.Context) (int, error) {
	select {
	case <-i.exited:
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.status.ExitCode(), nil
	case <-ctx.Done():
		return -1, fmt.Errorf("%s did not exit: %w", i.Name, ctx.Err())
	}
}

// WaitReady polls /health/ready until it answers 200.
func (i *Instance) WaitReady(ctx context.Context) error {
	for {
		if !i.Running() {
			return fmt.Errorf("%s exited before becoming ready; logs:\n%s", i.Name, i.Logs.String())
		}
		if i.Ready(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w; logs:\n%s", i.Name, ctx.Err(), i.Logs.String())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Ready reports whether /health/ready answers 200 right now.
func (i *Instance) Ready(ctx context.Context) bool {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+i.Addr+"/health/ready", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// WaitMessage blocks until the process has logged a line with this message.
func (i *Instance) WaitMessage(ctx context.Context, message string) error {
	for {
		for _, m := range i.Logs.Messages() {
			if m == message {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s never logged %q: %w; logs:\n%s", i.Name, message, ctx.Err(), i.Logs.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// DumpLog writes the process output to <dir>/<log name>.log for CI to upload.
func (i *Instance) DumpLog(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // directory chosen by the CI environment, not by input
		return err
	}
	// dir comes from the CI environment and the name from the test, not from input.
	return os.WriteFile(filepath.Join(dir, i.logName()+".log"), []byte(i.Logs.String()), 0o600) //nolint:gosec // path chosen by the test runner
}

// DumpInstanceLogs saves the output of every instance started in this run.
func DumpInstanceLogs(dir string) error {
	started.mu.Lock()
	instances := slices.Clone(started.instances)
	started.mu.Unlock()
	var errs []error
	for _, i := range instances {
		errs = append(errs, i.DumpLog(dir))
	}
	return errors.Join(errs...)
}
