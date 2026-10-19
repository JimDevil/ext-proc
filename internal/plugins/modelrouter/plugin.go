// Package modelrouter is migrated from the higress model-router plugin
// (higress-group/higress plugins/wasm-go/extensions/model-router).
//
// Semantics aligned with the original:
//   - only active on paths matching enableOnPathSuffix;
//   - JSON bodies: the model name is extracted via modelKey and injected into
//     modelToHeader; for "provider/model" names the provider part goes into
//     addProviderHeader, and with keepOriginalModelName=false the body model is
//     rewritten to the de-prefixed value;
//   - multipart bodies: the model form field follows the same injection/split;
//   - a model value of "higress/auto" rewrites the target model from the
//     autoRouting rules (matched against the last user message) or
//     defaultModel (both header and body).
package modelrouter

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	extproc "github.com/JimDevil/ext-proc/sdk"
	gfjson "github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// Shared-state keys between the headers and body phases (the body phase cannot
// read headers, so they must be stashed early).
const (
	sharedEnabledKey     = "modelrouter.enabled"
	sharedContentTypeKey = "modelrouter.content-type"
	sharedStreamedKey    = "modelrouter.streamed"
)

// ModelRouter is the model-routing plugin. Plugin instances are rebuilt whole
// on config hot reload — treat instances as immutable, no locking needed.
type ModelRouter struct {
	plugin.BasePlugin
	config *Config
	logger *slog.Logger
}

// New is the plugin factory.
func New() plugin.Plugin { return &ModelRouter{} }

// init self-registers the plugin.
func init() { plugin.Register("model-router", New) }

// Name implements plugin.Plugin.
func (m *ModelRouter) Name() string { return "model-router" }

// Init implements plugin.Plugin: parses the plugin-private config.
func (m *ModelRouter) Init(cfg *gfjson.Json, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	c, err := parseConfig(cfg, logger)
	if err != nil {
		return err
	}
	m.config = c
	m.logger = logger.With(slog.String("plugin", m.Name()))
	return nil
}

// OnRequestHeaders implements plugin.Plugin: checks whether the path is
// enabled and caches the content type for the body phase.
func (m *ModelRouter) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	path, _ := headers.Get(":path")

	// Strip the query string before suffix matching.
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	enabled := false
	for _, suffix := range m.config.enableOnPathSuffix {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			enabled = true
			break
		}
	}

	if err := ctx.SetValue(sharedEnabledKey, enabled); err != nil {
		return gerror.Wrap(err, "failed to cache the path gate")
	}
	if ct, ok := headers.Get("content-type"); ok {
		if err := ctx.SetValue(sharedContentTypeKey, ct); err != nil {
			return gerror.Wrap(err, "failed to cache the content type")
		}
	}
	return nil
}

// OnRequestBody implements plugin.Plugin: dispatches to the JSON / multipart
// handler by content type.
func (m *ModelRouter) OnRequestBody(ctx plugin.Context, body []byte) error {
	if enabled, ok := ctx.GetValue(sharedEnabledKey); !ok {
		return nil
	} else if v, isBool := enabled.(bool); !isBool || !v {
		return nil
	}

	// Model extraction needs the complete body. Under Envoy's STREAMED body
	// mode a single chunk is not a complete JSON/multipart document. Remember
	// that a partial chunk was seen and skip the final chunk too, instead of
	// rewriting a body that Envoy has already forwarded in pieces.
	if !ctx.EndOfStream() {
		if err := ctx.SetValue(sharedStreamedKey, true); err != nil {
			return gerror.Wrap(err, "failed to record a streamed body chunk")
		}
		return nil
	}
	if streamed, ok := ctx.GetValue(sharedStreamedKey); ok {
		if v, isBool := streamed.(bool); isBool && v {
			m.logger.Debug("model-router requires a buffered body; skipping a streamed request")
			return nil
		}
	}

	ct, _ := ctx.GetValue(sharedContentTypeKey)
	ctStr, _ := ct.(string)
	ctStr = strings.ToLower(strings.TrimSpace(ctStr))

	switch {
	case strings.Contains(ctStr, "application/json"):
		return m.handleJSONBody(ctx, body)
	case strings.Contains(ctStr, "multipart/form-data"):
		return m.handleMultipartBody(ctx, body, ctStr)
	}
	return nil
}

// handleJSONBody processes a JSON body (full semantics of higress's
// handleJsonBody).
func (m *ModelRouter) handleJSONBody(ctx plugin.Context, body []byte) error {
	if !json.Valid(body) {
		// Client-controlled malformed input is not a server error; logging it
		// at Error would allow trivial log amplification.
		m.logger.Debug("request body is not valid JSON, skipping")
		return nil
	}

	model := gjson.GetBytes(body, m.config.modelKey).String()
	if model == "" {
		return nil
	}
	// The model is attacker-controlled: never write a value containing CR/LF
	// or another control character into a header. Envoy would reject the
	// mutation, and it would be a header-smuggling primitive otherwise.
	if err := extproc.ValidateHeaderValue([]byte(model)); err != nil {
		m.logger.Warn("ignoring an invalid model value", slog.String("error", err.Error()))
		return nil
	}

	// Auto-routing branch: the magic value higress/auto rewrites the target
	// model from rules / defaultModel.
	if m.config.enableAutoRouting && model == autoModelPrefix {
		target := m.resolveAutoModel(body)
		if target == "" {
			m.logger.Debug("auto routing matched no rule and defaultModel is empty; keeping the request as-is")
			return nil
		}
		// modelToHeader empty means "do not inject a header" (the body is
		// still rewritten below); this matches the regular branch.
		if m.config.modelToHeader != "" {
			if err := ctx.OverwriteHeader(m.config.modelToHeader, extproc.HeaderValue{Value: target}); err != nil {
				return gerror.Wrap(err, "failed to inject the auto-routing model header")
			}
			ctx.ClearRouteCache()
		}
		newBody, err := sjson.SetBytes(body, m.config.modelKey, target)
		if err != nil {
			return gerror.Wrap(err, "failed to rewrite the body during auto routing")
		}
		m.logger.Debug("auto routing rewrote the target model", slog.String("target", target))
		return ctx.ReplaceBody(newBody)
	}

	// Regular branch: inject the model name into the routing header.
	if m.config.modelToHeader != "" {
		if err := ctx.OverwriteHeader(m.config.modelToHeader, extproc.HeaderValue{Value: model}); err != nil {
			return gerror.Wrap(err, "failed to inject the model routing header")
		}
		ctx.ClearRouteCache()
	}

	// provider/model prefix split.
	if m.config.addProviderHeader != "" {
		provider, rest, found := strings.Cut(model, "/")
		if !found {
			m.logger.Debug("model name has no provider prefix, skipping the provider header", slog.String("model", model))
			return nil
		}
		if err := ctx.OverwriteHeader(m.config.addProviderHeader, extproc.HeaderValue{Value: provider}); err != nil {
			return gerror.Wrap(err, "failed to inject the provider header")
		}
		ctx.ClearRouteCache()
		if !m.config.keepOriginalModelName {
			newBody, err := sjson.SetBytes(body, m.config.modelKey, rest)
			if err != nil {
				return gerror.Wrap(err, "failed to rewrite the body after the provider split")
			}
			m.logger.Debug("model name split by provider",
				slog.String("provider", provider), slog.String("model", rest))
			return ctx.ReplaceBody(newBody)
		}
	}
	return nil
}

// resolveAutoModel is the auto-routing decision: match the text of the last
// user message against the rules in order, falling back to defaultModel.
func (m *ModelRouter) resolveAutoModel(body []byte) string {
	if userMsg := extractLastUserMessage(body); userMsg != "" {
		for _, rule := range m.config.autoRoutingRules {
			if rule.pattern.MatchString(userMsg) {
				m.logger.Debug("auto-routing rule matched",
					slog.String("pattern", rule.pattern.String()), slog.String("model", rule.model))
				return rule.model
			}
		}
	}
	return m.config.defaultModel
}

// extractLastUserMessage extracts the text of the last role=user message in
// the messages array (also handles the array-content rich form, aligned with
// the higress implementation).
func extractLastUserMessage(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}

	var last string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		if content.IsArray() {
			for _, item := range content.Array() {
				if item.Get("type").String() == "text" {
					last = item.Get("text").String()
				}
			}
		} else {
			last = content.String()
		}
	}
	return last
}

// handleMultipartBody processes a multipart body: rewrites the model form
// field (aligned with higress's handleMultipartBody).
func (m *ModelRouter) handleMultipartBody(ctx plugin.Context, body []byte, contentType string) error {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return gerror.Wrap(err, "failed to parse the multipart content-type")
	}
	boundary, ok := params["boundary"]
	if !ok {
		m.logger.Error("multipart request has no boundary, skipping")
		return nil
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		return gerror.Wrap(err, "failed to set the multipart boundary")
	}

	modified := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return gerror.Wrap(err, "failed to read a multipart part")
		}
		content, err := io.ReadAll(part)
		if err != nil {
			return gerror.Wrap(err, "failed to read part content")
		}

		if part.FormName() == m.config.modelKey {
			model := string(content)
			if err := extproc.ValidateHeaderValue([]byte(model)); err != nil {
				m.logger.Warn("ignoring an invalid multipart model value", slog.String("error", err.Error()))
				if err := writePart(writer, part.Header, model); err != nil {
					return err
				}
				continue
			}
			if m.config.modelToHeader != "" {
				if err := ctx.OverwriteHeader(m.config.modelToHeader, extproc.HeaderValue{Value: model}); err != nil {
					return gerror.Wrap(err, "failed to inject the model routing header")
				}
				ctx.ClearRouteCache()
			}
			if m.config.addProviderHeader != "" {
				provider, rest, found := strings.Cut(model, "/")
				if found {
					if err := extproc.ValidateHeaderValue([]byte(provider)); err != nil {
						m.logger.Warn("ignoring an invalid multipart provider value", slog.String("error", err.Error()))
					} else if err := ctx.OverwriteHeader(m.config.addProviderHeader, extproc.HeaderValue{Value: provider}); err != nil {
						return gerror.Wrap(err, "failed to inject the provider header")
					}
					ctx.ClearRouteCache()
					if !m.config.keepOriginalModelName {
						// Rewrite this part's model value (other parts pass through).
						if err := writePart(writer, part.Header, rest); err != nil {
							return err
						}
						modified = true
						m.logger.Debug("multipart part split by provider",
							slog.String("provider", provider), slog.String("model", rest))
						continue
					}
				}
			}
		}

		if err := writePart(writer, part.Header, string(content)); err != nil {
			return err
		}
	}

	if err := writer.Close(); err != nil {
		return gerror.Wrap(err, "failed to finish the multipart writer")
	}
	if modified {
		return ctx.ReplaceBody(out.Bytes())
	}
	return nil
}

// writePart writes one multipart part (must reuse the outer writer so the
// boundary stays consistent).
func writePart(writer *multipart.Writer, header textproto.MIMEHeader, content string) error {
	w, err := writer.CreatePart(header)
	if err != nil {
		return gerror.Wrap(err, "failed to create a multipart part")
	}
	if _, err := w.Write([]byte(content)); err != nil {
		return gerror.Wrap(err, "failed to write part content")
	}
	return nil
}
