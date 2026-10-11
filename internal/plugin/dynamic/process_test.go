package dynamic

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/JimDevil/ext-proc/internal/plugin/plugintest"
)

// echoBin is the compiled cmd/echo-plugin fixture (built once by TestMain).
var echoBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "extproc-dyn-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tempdir:", err)
		os.Exit(1)
	}
	echoBin = filepath.Join(tmp, "echo-plugin")
	build := exec.Command("go", "build", "-o", echoBin, "./cmd/echo-plugin")
	build.Dir = "../../.."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build cmd/echo-plugin:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// spawnEcho starts the echo-plugin subprocess with the given config.
func spawnEcho(t *testing.T, cfg string) *managedProcess {
	t.Helper()
	proc, err := startProcess("echo-test", echoBin,
		[]string{"-listen", "tcp:127.0.0.1:0"},
		[]string{envPluginConfig + "=" + cfg}, testLog)
	if err != nil {
		t.Fatalf("startProcess: %v", err)
	}
	return proc
}

func TestSpawnAndPhaseRoundTrip(t *testing.T) {
	t.Parallel()
	proc := spawnEcho(t, `{"value":"on"}`)
	sp := &subprocessPlugin{name: "echo-test", proc: proc, timeout: 5 * time.Second, logger: testLog}

	mctx := plugintest.NewMockContext()
	if err := sp.OnRequestHeaders(mctx, allHeaders(":path", "/x")); err != nil {
		t.Fatalf("OnRequestHeaders: %v", err)
	}
	if v, ok := mctx.LastHeaderValue("x-echo"); !ok || v != "on" {
		t.Fatalf("x-echo not set by the subprocess: %v %v", v, ok)
	}

	// Close terminates the process (SIGTERM drain path).
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !proc.hasExited() {
		t.Fatal("subprocess should have exited after Close")
	}
}

func TestCrashRestart(t *testing.T) {
	t.Parallel()
	proc := spawnEcho(t, `{"value":"after-crash"}`)
	sp := &subprocessPlugin{name: "echo-test", proc: proc, timeout: 5 * time.Second, logger: testLog}
	defer sp.Close()

	// Kill the process behind the manager's back, as a real crash would.
	proc.mu.Lock()
	cmd := proc.cmd
	proc.mu.Unlock()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	<-proc.waitCh

	// The next phase call must revive the process and succeed.
	mctx := plugintest.NewMockContext()
	if err := sp.OnRequestHeaders(mctx, allHeaders(":path", "/x")); err != nil {
		t.Fatalf("OnRequestHeaders after crash: %v", err)
	}
	if v, ok := mctx.LastHeaderValue("x-echo"); !ok || v != "after-crash" {
		t.Fatalf("revived subprocess did not apply the config: %v %v", v, ok)
	}
	if proc.restarts < 1 {
		t.Fatalf("expected a restart to have happened, got %d", proc.restarts)
	}
}

func TestSubprocessCloseKillsStuckProcess(t *testing.T) {
	t.Parallel()
	proc := spawnEcho(t, `{}`)
	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !proc.hasExited() {
		t.Fatal("process should be gone after Close")
	}
}
