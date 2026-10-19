package apikeyextract

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	extproc "github.com/JimDevil/ext-proc/sdk"
	gfjson "github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
)

const (
	// defaultTargetHeader is the default header the key is injected into.
	defaultTargetHeader = "x-extracted-apikey"
	// bearerPrefix is the Bearer auth-scheme prefix of the Authorization header
	// (matched case-insensitively).
	bearerPrefix = "bearer "
)

// defaultSources is the extraction order when sources is not configured.
var defaultSources = []sourceConfig{
	{sourceType: "header", name: "x-api-key"},
	{sourceType: "header", name: "authorization", bearer: true},
	{sourceType: "query", name: "apikey"},
}

// sourceConfig is one extraction source.
type sourceConfig struct {
	// sourceType: header (request header) / query (URL query parameter).
	sourceType string
	// name is the header name (case-insensitive) or the query parameter name.
	name string
	// bearer requires the value to look like "Bearer <key>"; the prefix is
	// stripped to obtain the key. Values without the Bearer prefix skip this
	// source (so Basic and other auth schemes are never mistaken for an API key).
	bearer bool
}

// Config is the plugin-private config.
type Config struct {
	// sources are tried in order; the first non-empty hit wins.
	sources []sourceConfig
	// targetHeader is the header the extracted key is injected into.
	targetHeader string
	// stripSource deletes the key from the source header after a hit (to stop
	// it leaking upstream; query sources cannot be deleted).
	stripSource bool
	// required rejects the request when no source matches (false lets it pass).
	required bool
	// rejectStatus is the HTTP status used when rejecting.
	rejectStatus int32
	// rejectMessage is the error message used when rejecting.
	rejectMessage string
}

// parseConfig parses the plugin config from its config node; a nil cfg
// (missing section) means all defaults.
func parseConfig(cfg *gfjson.Json, logger *slog.Logger) (*Config, error) {
	c := &Config{
		targetHeader:  defaultTargetHeader,
		rejectStatus:  http.StatusUnauthorized,
		rejectMessage: "missing api key",
	}
	if cfg == nil {
		c.sources = slices.Clone(defaultSources)
		return c, nil
	}

	if v := cfg.Get("targetHeader").String(); v != "" {
		c.targetHeader = v
	}
	c.stripSource = cfg.Get("stripSource").Bool()
	c.required = cfg.Get("required").Bool()
	if node := cfg.Get("rejectStatus"); node != nil && !node.IsNil() {
		status := node.Int()
		if status < 100 || status > 599 {
			return nil, gerror.Newf("apikey-extract: rejectStatus %d is invalid (must be 100-599)", status)
		}
		c.rejectStatus = int32(status)
	}
	if v := cfg.Get("rejectMessage").String(); v != "" {
		c.rejectMessage = v
	}

	// A configured sources list replaces the defaults entirely. This is the
	// whole point of the option: operators decide the priority order and which
	// sources are trusted, so silently keeping the defaults in front of the
	// configured entries would make the option ineffective.
	sourcesNode := cfg.Get("sources")
	if sourcesNode == nil || sourcesNode.IsNil() {
		c.sources = slices.Clone(defaultSources)
	} else {
		for i, s := range cfg.GetJsons("sources") {
			sc := sourceConfig{
				sourceType: s.Get("type").String(),
				name:       s.Get("name").String(),
				bearer:     s.Get("bearer").Bool(),
			}
			if sc.sourceType != "header" && sc.sourceType != "query" {
				return nil, gerror.Newf("apikey-extract: sources[%d] has invalid type %q (must be header/query)", i, sc.sourceType)
			}
			if sc.name == "" {
				return nil, gerror.Newf("apikey-extract: sources[%d] has an empty name", i)
			}
			if sc.bearer && sc.sourceType != "header" {
				return nil, gerror.Newf("apikey-extract: sources[%d] bearer is only valid for header sources", i)
			}
			if sc.sourceType == "header" {
				validator := extproc.ValidateHeaderName
				if c.stripSource {
					// Removing the source must not target an Envoy-protected
					// header; fail at startup instead of every request.
					validator = extproc.ValidateMutableHeaderName
				}
				if err := validator(sc.name); err != nil {
					return nil, gerror.Wrapf(err, "apikey-extract: sources[%d]", i)
				}
			}
			c.sources = append(c.sources, sc)
		}
		if len(c.sources) == 0 {
			return nil, gerror.New("apikey-extract: the configured sources list must contain at least one entry")
		}
	}

	if err := extproc.ValidateMutableHeaderName(c.targetHeader); err != nil {
		return nil, gerror.Wrap(err, "apikey-extract: invalid targetHeader")
	}
	if err := extproc.ValidateStatus(c.rejectStatus); err != nil {
		return nil, gerror.Wrap(err, "apikey-extract: invalid rejectStatus")
	}
	if c.stripSource {
		for _, s := range c.sources {
			if s.sourceType == "header" && strings.EqualFold(s.name, c.targetHeader) {
				return nil, gerror.Newf("apikey-extract: stripSource would remove the target header %q; use a different source or targetHeader", c.targetHeader)
			}
		}
	}

	return c, nil
}
