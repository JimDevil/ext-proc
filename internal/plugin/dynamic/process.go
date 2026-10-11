// Package dynamic implements the DYNAMIC SUBPROCESS plugin runtime (route A):
// a plugin distributed as an OCI image (or a local binary) is spawned as a
// child process speaking the standard ext_proc protocol, and a thin adapter
// exposes it to the chain as a regular plugin.Plugin.
//
// The subprocess binary is built with internal/pluginhost — the same binary
// also works as an independent ext_proc service; only who dials it differs.
//
// Design notes:
//   - the wire protocol is ext_proc itself (no bespoke contract): the host
//     sends the FULL current phase state on a short-lived stream and applies
//     the returned mutations to the host-side Context;
//   - orchestration (order, matchRules, failurePolicy) stays in the host —
//     the subprocess only runs the plugin;
//   - every route-table instance (global + matchRule variants) owns its own
//     subprocess with the instance's merged config;
//   - subprocesses are reclaimed via the retired-chain TTL on hot reload
//     (see processor.SwapChain) and on graceful shutdown.
package dynamic

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	hpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/JimDevil/ext-proc/internal/pluginhost"
)

// spawnTimeout bounds one launch attempt (exec + handshake + dial + health).
const spawnTimeout = 15 * time.Second

// restartBackoff is the delay schedule between crash-restart attempts; after
// the last failed attempt the process stays down until the next phase call
// retries (the host chain's failurePolicy covers the requests in between).
var restartBackoff = []time.Duration{0, 100 * time.Millisecond, 400 * time.Millisecond, time.Second}

// envPluginConfig carries the plugin's private config node (inline JSON) to
// the subprocess; internal/pluginhost reads it before -config.
const envPluginConfig = "EXTPROC_PLUGIN_CONFIG"

// managedProcess owns one plugin subprocess: launch + handshake, the gRPC
// connection, crash restart on demand, and shutdown. Safe for concurrent use
// (plugin instances are called by many streams at once).
type managedProcess struct {
	name   string
	binary string
	args   []string
	env    []string
	logger *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	conn     *grpc.ClientConn
	exited   bool
	closed   bool
	waitCh   chan struct{}
	restarts int
}

// startProcess spawns the plugin binary and waits for its handshake.
func startProcess(name, binary string, args []string, extraEnv []string, logger *slog.Logger) (*managedProcess, error) {
	m := &managedProcess{name: name, binary: binary, args: args, env: extraEnv, logger: logger}
	if err := m.launch(); err != nil {
		return nil, err
	}
	return m, nil
}

// launch spawns the process and dials it. Callers hold m.mu.
func (m *managedProcess) launch() error {
	cmd := exec.Command(m.binary, m.args...)
	cmd.Env = append(os.Environ(), m.env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s: %w", m.binary, err)
	}
	m.cmd = cmd
	m.exited = false
	m.waitCh = make(chan struct{})

	// Subprocess logs stream into ours at debug level.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			m.logger.Debug("plugin stderr", slog.String("plugin", m.name), slog.String("line", sc.Text()))
		}
	}()

	// Wait for the one-line handshake (the listener is bound by then).
	addr, err := pluginhost.ReadHandshake(bufio.NewScanner(stdout), spawnTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		<-m.waitCh
		return err
	}

	conn, err := grpc.NewClient(dialTarget(addr), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = cmd.Process.Kill()
		<-m.waitCh
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	// Readiness: block until the health service answers (bounded), so a
	// half-started process never reaches the request path.
	hctx, hcancel := context.WithTimeout(context.Background(), spawnTimeout)
	defer hcancel()
	hc := hpb.NewHealthClient(conn)
	if _, err := hc.Check(hctx, &hpb.HealthCheckRequest{}, grpc.WaitForReady(true)); err != nil {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		<-m.waitCh
		return fmt.Errorf("plugin %s not ready at %s: %w", m.name, addr, err)
	}

	m.conn = conn

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.exited = true
		closed := m.closed
		m.mu.Unlock()
		close(m.waitCh)
		if err != nil && !closed {
			m.logger.Error("plugin process exited unexpectedly (it will be restarted on the next call)",
				slog.String("plugin", m.name), slog.String("error", err.Error()))
		}
	}()

	m.logger.Info("plugin subprocess started",
		slog.String("plugin", m.name), slog.String("binary", m.binary), slog.String("addr", addr))
	return nil
}

// externalClient returns a ready-to-use ExternalProcessor client, reviving a
// crashed process first.
func (m *managedProcess) externalClient() (epb.ExternalProcessorClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("plugin %s process is closed", m.name)
	}
	if m.conn == nil || m.exited {
		if err := m.reviveLocked(); err != nil {
			return nil, err
		}
	}
	return epb.NewExternalProcessorClient(m.conn), nil
}

// reviveLocked restarts a crashed process with bounded backoff. m.mu held.
func (m *managedProcess) reviveLocked() error {
	m.teardownLocked()

	var lastErr error
	for _, delay := range restartBackoff {
		if delay > 0 {
			time.Sleep(delay)
		}
		m.restarts++
		if err := m.launch(); err == nil {
			return nil
		} else {
			lastErr = err
			m.logger.Warn("plugin process restart failed",
				slog.String("plugin", m.name), slog.String("error", err.Error()))
		}
	}
	return fmt.Errorf("plugin %s failed to restart after crash: %w", m.name, lastErr)
}

// teardownLocked closes the connection and kills a leftover process. m.mu held.
func (m *managedProcess) teardownLocked() {
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	if m.cmd != nil && m.cmd.Process != nil {
		if !m.exited {
			_ = m.cmd.Process.Kill()
		}
		m.cmd = nil
	}
}

// Close shuts the subprocess down: SIGTERM with a bounded drain, then SIGKILL.
// Idempotent (a retired chain and process shutdown may both call it).
func (m *managedProcess) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	conn, cmd, waitCh := m.conn, m.cmd, m.waitCh
	m.conn = nil
	m.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	if cmd != nil && cmd.Process != nil {
		if !m.hasExited() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-waitCh:
			case <-time.After(5 * time.Second):
				m.logger.Warn("plugin process drain timeout, killing", slog.String("plugin", m.name))
				_ = cmd.Process.Kill()
				<-waitCh
			}
		}
	}
	return nil
}

// hasExited reports whether the current process has terminated.
func (m *managedProcess) hasExited() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exited
}

// dialTarget converts a handshake address ("tcp://host:port" or
// "unix:///path") into a gRPC target.
func dialTarget(addr string) string {
	if after, ok := strings.CutPrefix(addr, "unix://"); ok {
		return "unix://" + after
	}
	if after, ok := strings.CutPrefix(addr, "tcp://"); ok {
		// passthrough: the address is already final (localhost of this pod);
		// no resolver involvement wanted.
		return "passthrough:///" + after
	}
	return "passthrough:///" + addr
}
