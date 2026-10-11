package modelrouter

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"strings"
	"testing"

	extproc "github.com/JimDevil/ext-proc/sdk"
	gfjson "github.com/gogf/gf/v2/encoding/gjson"
	"github.com/tidwall/gjson"

	"github.com/JimDevil/ext-proc/internal/plugin/plugintest"
)

func newTestPlugin(t *testing.T, cfg map[string]any) *ModelRouter {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New()
	var node *gfjson.Json
	if cfg != nil {
		node = gfjson.New(cfg)
	}
	if err := p.Init(node, logger); err != nil {
		t.Fatalf("plugin init failed: %v", err)
	}
	return p.(*ModelRouter)
}

// runThrough executes the headers -> body phases, simulating one request.
func runThrough(t *testing.T, p *ModelRouter, path, contentType string, body []byte) *plugintest.MockContext {
	t.Helper()
	ctx := plugintest.NewMockContext()
	hdrs := extproc.AllHeaders{
		Headers: map[string][]string{
			":path":        {path},
			"content-type": {contentType},
		},
		RawHeaders: map[string][][]byte{
			":path":        {[]byte(path)},
			"content-type": {[]byte(contentType)},
		},
	}
	if err := p.OnRequestHeaders(ctx, hdrs); err != nil {
		t.Fatalf("OnRequestHeaders failed: %v", err)
	}
	if err := p.OnRequestBody(ctx, body); err != nil {
		t.Fatalf("OnRequestBody failed: %v", err)
	}
	return ctx
}

func TestModelRouterJSONBasics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  map[string]any
		body string

		// Expected routing header (empty name = expect no injection).
		wantHeaderName  string
		wantHeaderValue string
		// Expected provider header (empty name = expect no injection).
		wantProviderName  string
		wantProviderValue string
		// Expected body rewrite and the resulting model field.
		wantBodyRewritten bool
		wantBodyModel     string
	}{
		{
			name:            "injects the model routing header",
			cfg:             map[string]any{"modelToHeader": "x-higress-llm-model"},
			body:            `{"model":"glm-5.3","messages":[]}`,
			wantHeaderName:  "x-higress-llm-model",
			wantHeaderValue: "glm-5.3",
		},
		{
			name: "provider split rewrites the body",
			cfg: map[string]any{
				"modelToHeader":         "x-higress-llm-model",
				"addProviderHeader":     "x-provider",
				"keepOriginalModelName": false,
			},
			body:              `{"model":"ecloud/glm-5.3"}`,
			wantHeaderName:    "x-higress-llm-model",
			wantHeaderValue:   "ecloud/glm-5.3",
			wantProviderName:  "x-provider",
			wantProviderValue: "ecloud",
			wantBodyRewritten: true,
			wantBodyModel:     "glm-5.3",
		},
		{
			name: "provider split keeps the original model name",
			cfg: map[string]any{
				"modelToHeader":         "x-higress-llm-model",
				"addProviderHeader":     "x-provider",
				"keepOriginalModelName": true,
			},
			body:              `{"model":"ecloud/glm-5.3"}`,
			wantHeaderName:    "x-higress-llm-model",
			wantHeaderValue:   "ecloud/glm-5.3",
			wantProviderName:  "x-provider",
			wantProviderValue: "ecloud",
			wantBodyRewritten: false,
		},
		{
			name: "empty model leaves the request untouched",
			cfg:  map[string]any{"modelToHeader": "x-higress-llm-model"},
			body: `{"messages":[]}`,
		},
		{
			name: "invalid JSON is skipped",
			cfg:  map[string]any{"modelToHeader": "x-higress-llm-model"},
			body: `{invalid`,
		},
		{
			name: "no provider prefix means no provider header",
			cfg: map[string]any{
				"addProviderHeader": "x-provider",
			},
			body: `{"model":"glm-5.3"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, tt.cfg)
			ctx := runThrough(t, p, "/v1/chat/completions", "application/json", []byte(tt.body))

			// Routing header: disabled paths / empty model / invalid JSON must
			// not inject.
			if v, ok := ctx.LastHeaderValue("x-higress-llm-model"); tt.wantHeaderName != "" && (!ok || v != tt.wantHeaderValue) {
				t.Fatalf("routing header mismatch: got (%q,%v), want %q", v, ok, tt.wantHeaderValue)
			}
			// Provider header.
			if v, ok := ctx.LastHeaderValue("x-provider"); tt.wantProviderName != "" && (!ok || v != tt.wantProviderValue) {
				t.Fatalf("provider header mismatch: got (%q,%v), want %q", v, ok, tt.wantProviderValue)
			}

			body, rewritten := ctx.Body()
			if rewritten != tt.wantBodyRewritten {
				t.Fatalf("body rewrite mismatch: got %v", rewritten)
			}
			if tt.wantBodyRewritten {
				if m := gjson.GetBytes(body, "model").String(); m != tt.wantBodyModel {
					t.Fatalf("rewritten model mismatch: got %q, want %q", m, tt.wantBodyModel)
				}
			}
		})
	}
}

func TestModelRouterPathSuffixGate(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, map[string]any{"modelToHeader": "x-higress-llm-model"})

	// Non-enabled path: the body phase must be skipped entirely.
	ctx := runThrough(t, p, "/v1/other-api", "application/json", []byte(`{"model":"glm-5.3"}`))
	if _, ok := ctx.LastHeaderValue("x-higress-llm-model"); ok {
		t.Fatal("non-enabled path must not inject the routing header")
	}

	// A query string does not affect suffix matching.
	ctx = runThrough(t, p, "/v1/chat/completions?foo=bar", "application/json", []byte(`{"model":"glm-5.3"}`))
	if v, ok := ctx.LastHeaderValue("x-higress-llm-model"); !ok || v != "glm-5.3" {
		t.Fatalf("enabled path with query should still inject: %q %v", v, ok)
	}
}

func TestModelRouterAutoRouting(t *testing.T) {
	t.Parallel()

	autoCfg := func(defaultModel string) map[string]any {
		return map[string]any{
			"autoRouting": map[string]any{
				"enable":       true,
				"defaultModel": defaultModel,
				"rules": []any{
					// The rule matches Chinese text on purpose: it proves the
					// regex behaves correctly on unicode input.
					map[string]any{"pattern": "(?i)翻译|translate", "model": "ecloud/translate-model"},
				},
			},
		}
	}

	tests := []struct {
		name        string
		cfg         map[string]any
		body        string
		wantModel   string
		wantRewrite bool
	}{
		{
			name:        "rule matches the last user message",
			cfg:         autoCfg("ecloud/default-model"),
			body:        `{"model":"higress/auto","messages":[{"role":"system","content":"hi"},{"role":"user","content":"请帮我翻译这句话"}]}`,
			wantModel:   "ecloud/translate-model",
			wantRewrite: true,
		},
		{
			name:        "no rule match falls back to defaultModel",
			cfg:         autoCfg("ecloud/default-model"),
			body:        `{"model":"higress/auto","messages":[{"role":"user","content":"随便聊聊"}]}`,
			wantModel:   "ecloud/default-model",
			wantRewrite: true,
		},
		{
			name:        "no match and no defaultModel keeps the request as-is",
			cfg:         autoCfg(""),
			body:        `{"model":"higress/auto","messages":[{"role":"user","content":"随便聊聊"}]}`,
			wantModel:   "higress/auto",
			wantRewrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, tt.cfg)
			ctx := runThrough(t, p, "/v1/chat/completions", "application/json", []byte(tt.body))

			v, ok := ctx.LastHeaderValue("x-higress-llm-model")
			if tt.wantRewrite {
				if !ok || v != tt.wantModel {
					t.Fatalf("auto-routing header mismatch: got (%q,%v), want %q", v, ok, tt.wantModel)
				}
				body, rewritten := ctx.Body()
				if !rewritten {
					t.Fatal("auto routing should rewrite the body")
				}
				if m := gjson.GetBytes(body, "model").String(); m != tt.wantModel {
					t.Fatalf("rewritten model mismatch: got %q, want %q", m, tt.wantModel)
				}
			} else {
				if ok {
					t.Fatalf("routing header must not be injected: %q", v)
				}
				if _, rewritten := ctx.Body(); rewritten {
					t.Fatal("body must not be rewritten")
				}
			}
		})
	}
}

func TestModelRouterMultipart(t *testing.T) {
	t.Parallel()

	// Build a multipart body carrying a model form field.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_, _ = w.CreateFormFile("file", "audio.wav")
	fw, _ := w.CreateFormField("model")
	_, _ = fw.Write([]byte("ecloud/glm-5.3"))
	w.Close()

	p := newTestPlugin(t, map[string]any{
		"modelToHeader":     "x-higress-llm-model",
		"addProviderHeader": "x-provider",
	})
	ctx := runThrough(t, p, "/v1/audio/speech", "multipart/form-data; boundary="+w.Boundary(), buf.Bytes())

	if v, ok := ctx.LastHeaderValue("x-higress-llm-model"); !ok || v != "ecloud/glm-5.3" {
		t.Fatalf("multipart routing header mismatch: got (%q,%v)", v, ok)
	}
	if v, ok := ctx.LastHeaderValue("x-provider"); !ok || v != "ecloud" {
		t.Fatalf("multipart provider header mismatch: got (%q,%v)", v, ok)
	}

	// The plugin preserves the original boundary; the body should be rewritten
	// with the model field de-prefixed.
	body, rewritten := ctx.Body()
	if !rewritten {
		t.Fatal("multipart body should be rewritten")
	}
	r := multipart.NewReader(bytes.NewReader(body), w.Boundary())
	found := false
	for {
		part, err := r.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			var sb strings.Builder
			if _, err := io.Copy(&sb, part); err != nil {
				t.Fatalf("failed to read the rewritten part: %v", err)
			}
			if got := sb.String(); got != "glm-5.3" {
				t.Fatalf("multipart rewritten model mismatch: got %q", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("model field not found in the rewritten body")
	}
}
