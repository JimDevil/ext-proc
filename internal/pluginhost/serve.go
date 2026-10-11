// Package pluginhost is the subprocess side of a dynamic plugin (route A):
// it turns ONE in-process plugin.Plugin into a standalone ext_proc service.
//
// The host process (internal/plugin/dynamic) spawns the plugin binary, which
// serves the standard ext_proc protocol on an ephemeral listener and reports
// its address with a one-line stdout handshake:
//
//	EXTPROC_PLUGIN_V1 <addr>
//
// Because the wire protocol IS ext_proc, the very same plugin binary also
// works as an independent ext_proc service (the split-N deployment) — the two
// forms differ only in who dials it and how.
//
// A plugin binary is a tiny main that blank-imports its plugin package and
// calls MustServe:
//
//	func main() {
//	    _ "github.com/JimDevil/ext-proc/internal/plugins/modelrouter"
//	    pluginhost.MustServe("model-router", slog.New(slog.NewTextHandler(os.Stderr, nil)))
//	}
//
// Flags: -listen "tcp:127.0.0.1:0" (default) or "unix:/path/plugin.sock";
// -config <path> with the plugin's private config node (YAML/JSON). The
// config node can also arrive via the EXTPROC_PLUGIN_CONFIG env var (inline
// JSON) — that is how the host passes per-instance merged configs.
package pluginhost

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gogf/gf/v2/encoding/gjson"

	"github.com/JimDevil/ext-proc/internal/plugin"
	extproc "github.com/JimDevil/ext-proc/sdk"
)

// HandshakePrefix is the first token of the one-line stdout handshake a plugin
// process prints once its listener is bound.
const HandshakePrefix = "EXTPROC_PLUGIN_V1"

// Serve resolves the registered plugin by name, initializes it from
// -config / EXTPROC_PLUGIN_CONFIG, serves the ext_proc protocol on the
// -listen address and blocks. On SIGTERM/SIGINT it stops gracefully with a
// bounded drain. pluginName must match the plugin's registered name.
func Serve(pluginName string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("extproc-plugin", flag.ContinueOnError)
	listen := fs.String("listen", "tcp:127.0.0.1:0", `listen address: "tcp:host:port" or "unix:/path/plugin.sock"`)
	configPath := fs.String("config", "", `plugin config file (the file content IS the plugin's private config node)`)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	f, ok := plugin.Lookup(pluginName)
	if !ok {
		return fmt.Errorf("plugin %s is not registered; blank-import its package in main()", pluginName)
	}
	p := f()

	cfg, err := loadConfigNode(*configPath)
	if err != nil {
		return fmt.Errorf("load plugin config: %w", err)
	}
	if err := p.Init(cfg, logger); err != nil {
		return fmt.Errorf("plugin %s Init failed: %w", pluginName, err)
	}

	network, address, err := parseListen(*listen)
	if err != nil {
		return err
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}

	proc := newSingleProcessor(pluginName, p, logger)
	srv := extproc.AssembleExtProcServer(extproc.NewDefaultServerOptions(), proc, logger)

	// Handshake AFTER the listener is bound, so the host can dial immediately.
	fmt.Printf("%s\t%s://%s\n", HandshakePrefix, lis.Addr().Network(), lis.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		return err
	case <-sig:
		// Bounded drain: in-flight streams finish, then force.
		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
			logger.Info("plugin process drained and stopped")
		case <-time.After(5 * time.Second):
			logger.Warn("drain timeout, forcing stop")
			srv.Stop()
		}
		_ = p.Close()
		return nil
	}
}

// MustServe is the Must variant of Serve: exits non-zero on error.
func MustServe(pluginName string, logger *slog.Logger) {
	if err := Serve(pluginName, logger); err != nil {
		logger.Error("plugin process failed", slog.String("plugin", pluginName), slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// loadConfigNode resolves the plugin's private config node: inline JSON from
// EXTPROC_PLUGIN_CONFIG wins, then -config <file> (YAML/JSON content); absent
// both, the node is nil and the plugin falls back to its defaults.
func loadConfigNode(configPath string) (*gjson.Json, error) {
	if envCfg := os.Getenv("EXTPROC_PLUGIN_CONFIG"); envCfg != "" {
		return gjson.LoadContent([]byte(envCfg))
	}
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, err
		}
		return gjson.LoadContent(data)
	}
	return nil, nil
}

// parseListen splits a -listen value into network and address.
func parseListen(s string) (network, address string, err error) {
	switch {
	case strings.HasPrefix(s, "tcp:"):
		return "tcp", strings.TrimPrefix(s, "tcp:"), nil
	case strings.HasPrefix(s, "unix:"):
		return "unix", strings.TrimPrefix(s, "unix:"), nil
	default:
		return "tcp", s, nil
	}
}

// ReadHandshake waits for the plugin process's handshake line and returns the
// dialable address ("<network>://<addr>", e.g. "tcp://127.0.0.1:54321" or
// "unix:///run/ext-proc/plugin.sock"). Host-side helper, kept next to the
// printer so the wire format stays in one place.
func ReadHandshake(stdout *bufio.Scanner, deadline time.Duration) (string, error) {
	deadlineC := time.After(deadline)
	lines := make(chan string, 1)
	go func() {
		for stdout.Scan() {
			line := strings.TrimSpace(stdout.Text())
			if after, ok := strings.CutPrefix(line, HandshakePrefix); ok {
				lines <- strings.TrimSpace(after)
				return
			}
		}
		lines <- "" // EOF without handshake
	}()
	select {
	case addr := <-lines:
		if addr == "" {
			return "", fmt.Errorf("plugin process exited before printing the %s handshake", HandshakePrefix)
		}
		return addr, nil
	case <-deadlineC:
		return "", fmt.Errorf("timed out after %s waiting for the %s handshake", deadline, HandshakePrefix)
	}
}
