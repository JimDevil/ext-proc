// Command echo-plugin is the minimal dynamic plugin example and the e2e test
// fixture for the subprocess plugin runtime: in the request-headers phase it
// overwrites the x-echo header with its config value and can optionally
// short-circuit the request.
//
// Config node (file or EXTPROC_PLUGIN_CONFIG inline JSON):
//
//	{ "value": "on", "reject": 403 }   // reject != 0 → ImmediateResponse
package main

import (
	"log/slog"
	"os"

	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/pluginhost"
	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"
)

type echoPlugin struct {
	plugin.BasePlugin
	value  string
	reject int32
}

func (p *echoPlugin) Name() string { return "echo-test" }

func (p *echoPlugin) Init(cfg *gjson.Json, _ *slog.Logger) error {
	if cfg == nil {
		return nil
	}
	p.value = cfg.Get("value").String()
	if c := cfg.Get("reject"); c != nil && !c.IsNil() {
		p.reject = int32(c.Int())
	}
	return nil
}

func (p *echoPlugin) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	if p.reject != 0 {
		return ctx.CancelRequest(p.reject, nil, "rejected by echo-plugin")
	}
	if p.value != "" {
		return ctx.OverwriteHeader("x-echo", extproc.HeaderValue{Value: p.value})
	}
	return nil
}

func init() { plugin.Register("echo-test", func() plugin.Plugin { return &echoPlugin{} }) }

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pluginhost.MustServe("echo-test", logger)
}
