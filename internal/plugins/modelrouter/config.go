package modelrouter

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
)

const (
	// autoModelPrefix is the magic model name that triggers auto routing
	// (aligned with the higress convention).
	autoModelPrefix = "higress/auto"
	// defaultModelHeader is the routing header used when modelToHeader is not
	// configured at all (aligned with the higress default). Set it explicitly
	// to "" to disable header injection.
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
		modelToHeader:      defaultModelHeader,
		enableOnPathSuffix: slices.Clone(defaultPathSuffixes),
	}
	if cfg == nil {
		return c, nil
	}

	if v := cfg.Get("modelKey").String(); v != "" {
		c.modelKey = v
	}
	// Distinguish "key absent" (use the higress-compatible default header)
	// from "key explicitly empty" (disable header injection entirely).
	if node := cfg.Get("modelToHeader"); node == nil || node.IsNil() {
		c.modelToHeader = defaultModelHeader
	} else {
		c.modelToHeader = node.String()
	}
	c.addProviderHeader = cfg.Get("addProviderHeader").String()
	c.keepOriginalModelName = cfg.Get("keepOriginalModelName").Bool()

	if err := validateHeaderOption("modelToHeader", c.modelToHeader); err != nil {
		return nil, err
	}
	if err := validateHeaderOption("addProviderHeader", c.addProviderHeader); err != nil {
		return nil, err
	}

	if suffixes := cfg.Get("enableOnPathSuffix"); suffixes != nil && len(suffixes.Strings()) > 0 {
		c.enableOnPathSuffix = slices.Clone(suffixes.Strings())
	}

	// Auto routing: a rule that fails to compile is skipped with a warning
	// (lenient, aligned with higress) so one bad rule never blocks the gateway.
	if ar := cfg.GetJson("autoRouting"); ar != nil {
		c.enableAutoRouting = ar.Get("enable").Bool()
		c.defaultModel = ar.Get("defaultModel").String()
		if err := validateHeaderValueOption("autoRouting.defaultModel", c.defaultModel); err != nil {
			return nil, err
		}
		for i, rule := range ar.GetJsons("rules") {
			pattern := rule.Get("pattern").String()
			model := rule.Get("model").String()
			if pattern == "" || model == "" {
				return nil, gerror.Newf("model-router: autoRouting.rules[%d] must set both pattern and model", i)
			}
			if err := validateHeaderValueOption(fmt.Sprintf("autoRouting.rules[%d].model", i), model); err != nil {
				return nil, err
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, gerror.Wrapf(err, "model-router: autoRouting.rules[%d] has an invalid pattern", i)
			}
			c.autoRoutingRules = append(c.autoRoutingRules, autoRule{pattern: re, model: model})
		}
	}

	if c.enableAutoRouting && c.defaultModel == "" && len(c.autoRoutingRules) == 0 {
		logger.Warn("auto routing enabled but neither rules nor defaultModel configured; higress/auto requests stay untouched")
	}
	return c, nil
}

// validateHeaderOption rejects a configured header name that Envoy would not
// accept. Empty means "feature disabled" and is always valid.
func validateHeaderOption(field, name string) error {
	if name == "" {
		return nil
	}
	if err := extproc.ValidateMutableHeaderName(name); err != nil {
		return gerror.Wrapf(err, "model-router: invalid %s", field)
	}
	return nil
}

// validateHeaderValueOption rejects a configured value that cannot be written
// into an HTTP header.
func validateHeaderValueOption(field, value string) error {
	if value == "" {
		return nil
	}
	if err := extproc.ValidateHeaderValue([]byte(value)); err != nil {
		return gerror.Wrapf(err, "model-router: invalid %s", field)
	}
	return nil
}
