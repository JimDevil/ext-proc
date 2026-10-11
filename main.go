// ext-proc entry point.
// Startup: boot.Init loads the config and assembles the chain, then server.Run
// serves the ext_proc gRPC service (SIGTERM/SIGINT trigger the SDK's built-in
// graceful shutdown).
package main

import (
	"github.com/gogf/gf/v2/os/gctx"

	"github.com/JimDevil/ext-proc/internal/boot"
	"github.com/JimDevil/ext-proc/internal/server"

	// Plugin self-registration: append an anonymous import here when adding a
	// new plugin.
	_ "github.com/JimDevil/ext-proc/internal/plugins/apikeyextract"
	_ "github.com/JimDevil/ext-proc/internal/plugins/modelrouter"
)

func main() {
	ctx := gctx.GetInitCtx()

	if err := boot.Init(ctx); err != nil {
		panic(err)
	}

	if err := server.Run(ctx); err != nil {
		panic(err)
	}
}
