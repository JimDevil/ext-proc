// Command model-router-plugin serves the in-repo model-router plugin as a
// standalone ext_proc process — the reference dynamic plugin: the SAME binary
// works as a subprocess plugin (spawned by the ext-proc host via
// chain.plugins[].url) and as an independent ext_proc service (a network
// deployment); only who dials it differs.
//
// Usage (spawned by the host, flags supplied by it):
//
//	model-router-plugin -listen tcp:127.0.0.1:0 -config /path/to/plugin.yaml
//
// or standalone:
//
//	model-router-plugin -listen tcp:0.0.0.0:9001 -config config.yaml
package main

import (
	"log/slog"
	"os"

	"github.com/JimDevil/ext-proc/internal/pluginhost"

	// Plugin self-registration.
	_ "github.com/JimDevil/ext-proc/internal/plugins/modelrouter"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pluginhost.MustServe("model-router", logger)
}
