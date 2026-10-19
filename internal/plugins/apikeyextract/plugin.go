// Package apikeyextract is migrated from the apikey-extract plugin: it
// extracts an API key from multiple sources and injects it into a target
// request header.
//
// Note: the original production plugin's config was unavailable (the test
// environment was unreachable at migration time), so this implementation is
// config-driven by design. The default source order is the x-api-key header →
// Authorization: Bearer → the apikey query parameter. Verify the sources order
// and value semantics in manifest/config/config.yaml against production before
// rolling out.
package apikeyextract

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	extproc "github.com/JimDevil/ext-proc/sdk"
	gfjson "github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// ApiKeyExtract is the API-key extraction plugin. Plugin instances are rebuilt
// whole on config hot reload — treat instances as immutable, no locking needed.
type ApiKeyExtract struct {
	plugin.BasePlugin
	config *Config
	logger *slog.Logger
}

// New is the plugin factory.
func New() plugin.Plugin { return &ApiKeyExtract{} }

// init self-registers the plugin.
func init() { plugin.Register("apikey-extract", New) }

// Name implements plugin.Plugin.
func (a *ApiKeyExtract) Name() string { return "apikey-extract" }

// Init implements plugin.Plugin: parses the plugin-private config.
func (a *ApiKeyExtract) Init(cfg *gfjson.Json, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	c, err := parseConfig(cfg, logger)
	if err != nil {
		return err
	}
	a.config = c
	a.logger = logger.With(slog.String("plugin", a.Name()))
	return nil
}

// OnRequestHeaders implements plugin.Plugin: extracts and injects in the
// request-headers phase (injecting here lets later plugins and routing see the
// key immediately).
func (a *ApiKeyExtract) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	key, source, found := a.extract(headers)
	if !found {
		if a.config.required {
			// required mode rejects on a miss; the response body is JSON-
			// encoded so a config value can never inject content.
			body, err := json.Marshal(struct {
				Error string `json:"error"`
			}{Error: a.config.rejectMessage})
			if err != nil {
				return gerror.Wrap(err, "failed to build the rejection body")
			}
			a.logger.Warn("no API key extracted, rejecting per the required policy",
				slog.Int("rejectStatus", int(a.config.rejectStatus)))
			return ctx.CancelRequest(a.config.rejectStatus,
				map[string]extproc.HeaderValue{"content-type": {Value: "application/json"}},
				string(body))
		}
		a.logger.Debug("no API key extracted, letting the request pass")
		return nil
	}

	if err := ctx.OverwriteHeader(a.config.targetHeader, extproc.HeaderValue{Value: key}); err != nil {
		return gerror.Wrapf(err, "failed to inject the target header %s", a.config.targetHeader)
	}

	// Only header sources support removal; a query lives in the :path pseudo
	// header and cannot be removed separately by protocol.
	if a.config.stripSource && source.sourceType == "header" {
		if err := ctx.RemoveHeader(source.name); err != nil {
			return gerror.Wrapf(err, "failed to remove the source header %s", source.name)
		}
	}
	return nil
}

// extract returns the first non-empty key across sources in order, together
// with the source that matched.
func (a *ApiKeyExtract) extract(headers extproc.AllHeaders) (string, sourceConfig, bool) {
	for _, s := range a.config.sources {
		for _, v := range sourceValues(headers, s) {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if s.bearer {
				// Only "Bearer <key>" qualifies; other auth schemes (Basic etc.)
				// are skipped and the next source is tried.
				if len(v) <= len(bearerPrefix) || !strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
					continue
				}
				v = strings.TrimSpace(v[len(bearerPrefix):])
				if v == "" {
					continue
				}
			}
			// Query parameters are percent-decoded and come from the client;
			// never write control characters into a header.
			if err := extproc.ValidateHeaderValue([]byte(v)); err != nil {
				a.logger.Debug("skipping an API key candidate with an invalid header value",
					slog.String("sourceType", s.sourceType), slog.String("source", s.name))
				continue
			}
			a.logger.Debug("extracted API key",
				slog.String("sourceType", s.sourceType), slog.String("source", s.name))
			return v, s, true
		}
	}
	return "", sourceConfig{}, false
}

// sourceValues collects all candidate values for one source (a header may
// appear multiple times; every occurrence is a candidate).
func sourceValues(headers extproc.AllHeaders, s sourceConfig) []string {
	switch s.sourceType {
	case "header":
		return headers.Values(s.name)
	case "query":
		path, ok := headers.Get(":path")
		if !ok {
			return nil
		}
		idx := strings.Index(path, "?")
		if idx == -1 {
			return nil
		}
		q, err := url.ParseQuery(path[idx+1:])
		if err != nil {
			// A parse failure counts as "no value from this source" and never
			// interrupts the request.
			return nil
		}
		return q[s.name]
	}
	return nil
}
