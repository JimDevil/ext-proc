package apikeyextract

import (
	"log/slog"
	"net/http"

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
		sources:       defaultSources,
	}
	if cfg == nil {
		return c, nil
	}

	if v := cfg.Get("targetHeader").String(); v != "" {
		c.targetHeader = v
	}
	c.stripSource = cfg.Get("stripSource").Bool()
	c.required = cfg.Get("required").Bool()
	if s := cfg.Get("rejectStatus").Int(); s > 0 {
		c.rejectStatus = int32(s)
	}
	if v := cfg.Get("rejectMessage").String(); v != "" {
		c.rejectMessage = v
	}

	for _, s := range cfg.GetJsons("sources") {
		sc := sourceConfig{
			sourceType: s.Get("type").String(),
			name:       s.Get("name").String(),
			bearer:     s.Get("bearer").Bool(),
		}
		if sc.sourceType != "header" && sc.sourceType != "query" {
			logger.Warn("ignoring an invalid extraction source (type must be header/query)",
				slog.String("type", sc.sourceType), slog.String("name", sc.name))
			continue
		}
		if sc.name == "" {
			logger.Warn("ignoring an invalid extraction source (empty name)", slog.String("type", sc.sourceType))
			continue
		}
		c.sources = append(c.sources, sc)
	}
	if len(c.sources) == 0 {
		return nil, gerror.New("apikey-extract: the configured sources list has no valid entries")
	}

	return c, nil
}
