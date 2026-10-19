// ext-proc entry point.
// Startup: boot.Init loads the config and assembles the chain, then server.Run
// serves the ext_proc gRPC service (SIGTERM/SIGINT trigger the SDK's built-in
// graceful shutdown).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gogf/gf/v2/os/gctx"

	"github.com/JimDevil/ext-proc/internal/boot"
	"github.com/JimDevil/ext-proc/internal/server"

	// Plugin self-registration: append an anonymous import here when adding a
	// new plugin.
	_ "github.com/JimDevil/ext-proc/internal/plugins/apikeyextract"
	_ "github.com/JimDevil/ext-proc/internal/plugins/modelrouter"
)

// Build metadata, injected with -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Scratch images have no shell/curl; expose a small healthcheck command
	// for Docker/Kubernetes exec probes.
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		os.Exit(runHealthcheck())
	}

	ctx := gctx.GetInitCtx()

	if err := boot.Init(ctx); err != nil {
		slog.Error("bootstrap failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	slog.Info("ext_proc starting",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("buildDate", date))

	if err := server.Run(ctx); err != nil {
		slog.Error("ext_proc exited with an error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// runHealthcheck performs one HTTP liveness request against the local metrics
// port. EXT_PROC_HEALTHCHECK_URL overrides the default URL.
func runHealthcheck() int {
	url := os.Getenv("EXT_PROC_HEALTHCHECK_URL")
	if url == "" {
		url = "http://127.0.0.1:19090/healthz"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
