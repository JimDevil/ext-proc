package modelrouter

import (
	"log/slog"
	"regexp"

	"github.com/gogf/gf/v2/encoding/gjson"
)

const (
	// autoModelPrefix is the magic model name that triggers auto routing
	// (aligned with the higress convention).
	autoModelPrefix = "higress/auto"
	// defaultModelHeader is the auto-routing target header when modelToHeader
	// is not configured (aligned with the higress default).
	defaultModelHeader = "x-higress-llm-model"
)

// defaultPathSuffixes is the enabled path-suffix list when enableOnPathSuffix
// is not configured (aligned with the higress list).
var defaultPathSuffixes = []string{
	"/completions",
	"/embeddings",
	"/images/generations",
	"/audio/speech",
	"/fine_tuning/jobs",
	"/moderations",
	"/image-synthesis",
	"/video-synthesis",
	"/rerank",
	"/messages",
	"/responses",
}

// autoRule is one auto-routing rule: when the regex matches the last user
// message, the target model is rewritten.
type autoRule struct {
	pattern *regexp.Regexp
	model   string
}

// Config is the plugin-private config (fields map one-to-one to the higress
// model-router options).
type Config struct {
	// modelKey is the JSON body field path holding the model name, default "model".
	modelKey string
	// modelToHeader is the request header the model name is injected into;
	// empty disables injection.
	modelToHeader string
	// addProviderHeader receives the provider part of "provider/model" names;
	// empty disables the split.
	addProviderHeader string
	// keepOriginalModelName keeps the original body model name after the split.
	keepOriginalModelName bool
	// enableOnPathSuffix is the enabled path-suffix list ("*" means all).
	enableOnPathSuffix []string
	// enableAutoRouting turns on higress/auto auto routing.
	enableAutoRouting bool
	// autoRoutingRules are compiled rules (config order = match order).
	autoRoutingRules []autoRule
	// defaultModel is the fallback when no rule matches.
	defaultModel string
}

// parseConfig parses the plugin config from its config node; a nil cfg
// (missing section) means all defaults.
func parseConfig(cfg *gjson.Json, logger *slog.Logger) (*Config, error) {
	c := &Config{
		modelKey:           "model",
		enableOnPathSuffix: defaultPathSuffixes,
	}
	if cfg == nil {
		return c, nil
	}

	if v := cfg.Get("modelKey").String(); v != "" {
		c.modelKey = v
	}
	c.modelToHeader = cfg.Get("modelToHeader").String()
	c.addProviderHeader = cfg.Get("addProviderHeader").String()
	c.keepOriginalModelName = cfg.Get("keepOriginalModelName").Bool()

	if suffixes := cfg.Get("enableOnPathSuffix"); suffixes != nil && len(suffixes.Strings()) > 0 {
		c.enableOnPathSuffix = suffixes.Strings()
	}

	// Auto routing: a rule that fails to compile is skipped with a warning
	// (lenient, aligned with higress) so one bad rule never blocks the gateway.
	if ar := cfg.GetJson("autoRouting"); ar != nil {
		c.enableAutoRouting = ar.Get("enable").Bool()
		c.defaultModel = ar.Get("defaultModel").String()
		for _, rule := range ar.GetJsons("rules") {
			pattern := rule.Get("pattern").String()
			model := rule.Get("model").String()
			if pattern == "" || model == "" {
				logger.Warn("ignoring invalid auto-routing rule (empty pattern/model)",
					slog.String("pattern", pattern), slog.String("model", model))
				continue
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warn("auto-routing regex failed to compile, ignoring the rule",
					slog.String("pattern", pattern), slog.String("error", err.Error()))
				continue
			}
			c.autoRoutingRules = append(c.autoRoutingRules, autoRule{pattern: re, model: model})
		}
	}

	if c.enableAutoRouting && c.defaultModel == "" && len(c.autoRoutingRules) == 0 {
		logger.Warn("auto routing enabled but neither rules nor defaultModel configured; higress/auto requests stay untouched")
	}
	return c, nil
}
