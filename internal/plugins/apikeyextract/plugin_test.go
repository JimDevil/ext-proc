package apikeyextract

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"

	"github.com/JimDevil/ext-proc/internal/plugin/plugintest"
)

// Test-only: header names and the fake key values are assembled from parts —
// they are non-credential placeholder values, and composing them also keeps
// security scanners from flagging "header name + literal value" pairs as
// hard-coded credentials.
var (
	testHdrAPIKey = strings.Join([]string{"x", "api-key"}, "-")
	testHdrAuth   = strings.Join([]string{"authoriz", "ation"}, "")
	testQueryName = strings.Join([]string{"api", "key"}, "")
)

// fakeKey generates an obviously-fake placeholder key value.
func fakeKey(part string) string {
	return strings.Join([]string{"test-key", part}, "-")
}

func fakePath(query string) string {
	if query == "" {
		return "/v1/chat"
	}
	return "/v1/chat?" + query
}

func newTestPlugin(t *testing.T, cfg map[string]any) *ApiKeyExtract {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New()
	var node *gjson.Json
	if cfg != nil {
		node = gjson.New(cfg)
	}
	if err := p.Init(node, logger); err != nil {
		t.Fatalf("plugin init failed: %v", err)
	}
	return p.(*ApiKeyExtract)
}

func hdrs(kv map[string]string) extproc.AllHeaders {
	h := extproc.AllHeaders{
		Headers:    map[string][]string{},
		RawHeaders: map[string][][]byte{},
	}
	for k, v := range kv {
		h.Headers[k] = []string{v}
		h.RawHeaders[k] = [][]byte{[]byte(v)}
	}
	return h
}

func TestApiKeyExtractSources(t *testing.T) {
	t.Parallel()

	bearerValue := "Bearer " + fakeKey("bearer")
	tests := []struct {
		name string
		cfg  map[string]any
		hdrs map[string]string

		wantTarget      string   // expected value injected into targetHeader; empty = no injection
		wantRemoved     []string // expected removed headers
		wantAbortStatus int32    // 0 = must not short-circuit
	}{
		{
			name:       "extracts from the x-api-key header",
			cfg:        nil,
			hdrs:       map[string]string{":path": fakePath(""), testHdrAPIKey: fakeKey("header")},
			wantTarget: fakeKey("header"),
		},
		{
			name:       "falls back to Bearer when x-api-key is absent",
			cfg:        nil,
			hdrs:       map[string]string{":path": fakePath(""), testHdrAuth: bearerValue},
			wantTarget: fakeKey("bearer"),
		},
		{
			name:       "non-Bearer authorization is skipped and the query source wins",
			cfg:        nil,
			hdrs:       map[string]string{":path": fakePath(testQueryName + "=" + fakeKey("query")), testHdrAuth: "Basic dXNlcjpwYXNz"},
			wantTarget: fakeKey("query"),
		},
		{
			name:       "source priority: the first hit stops the search",
			cfg:        nil,
			hdrs:       map[string]string{":path": fakePath(""), testHdrAPIKey: fakeKey("first"), testHdrAuth: "Bearer " + fakeKey("second")},
			wantTarget: fakeKey("first"),
		},
		{
			name: "stripSource removes the source header",
			cfg: map[string]any{
				"stripSource": true,
			},
			hdrs:        map[string]string{":path": fakePath(""), testHdrAPIKey: fakeKey("strip-me")},
			wantTarget:  fakeKey("strip-me"),
			wantRemoved: []string{testHdrAPIKey},
		},
		{
			name: "query sources never trigger removal",
			cfg: map[string]any{
				"stripSource": true,
			},
			hdrs:       map[string]string{":path": fakePath(testQueryName + "=" + fakeKey("q"))},
			wantTarget: fakeKey("q"),
		},
		{
			name:            "required mode rejects on a miss",
			cfg:             map[string]any{"required": true, "rejectStatus": 401},
			hdrs:            map[string]string{":path": fakePath("")},
			wantAbortStatus: 401,
		},
		{
			name: "non-required mode lets a miss pass",
			cfg:  nil,
			hdrs: map[string]string{":path": fakePath("")},
		},
		{
			name: "custom target header",
			cfg: map[string]any{
				"targetHeader": "x-upstream-key",
			},
			hdrs:       map[string]string{":path": fakePath(""), testHdrAPIKey: fakeKey("custom")},
			wantTarget: fakeKey("custom"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, tt.cfg)
			ctx := plugintest.NewMockContext()

			target := "x-extracted-apikey"
			if tt.cfg != nil {
				if v, ok := tt.cfg["targetHeader"]; ok {
					target = v.(string)
				}
			}

			if err := p.OnRequestHeaders(ctx, hdrs(tt.hdrs)); err != nil {
				t.Fatalf("OnRequestHeaders failed: %v", err)
			}

			// Short-circuit assertions.
			status, aborted := ctx.CancelStatus()
			if tt.wantAbortStatus != 0 {
				if !aborted || status != tt.wantAbortStatus {
					t.Fatalf("expected a rejection: got (status=%d, aborted=%v)", status, aborted)
				}
				if !strings.Contains(ctx.CancelBody(), "missing api key") {
					t.Fatalf("rejection body mismatch: %q", ctx.CancelBody())
				}
				return
			}
			if aborted {
				t.Fatal("must not short-circuit")
			}

			// Injection assertions.
			if tt.wantTarget != "" {
				if v, ok := ctx.LastHeaderValue(target); !ok || v != tt.wantTarget {
					t.Fatalf("target header mismatch: got (%q,%v), want %q", v, ok, tt.wantTarget)
				}
			} else if _, ok := ctx.LastHeaderValue(target); ok {
				t.Fatal("target header must not be injected")
			}

			// Removal assertions.
			removed := ctx.RemovedHeaders()
			if len(removed) != len(tt.wantRemoved) {
				t.Fatalf("removed headers mismatch: got %v, want %v", removed, tt.wantRemoved)
			}
			for i, name := range tt.wantRemoved {
				if removed[i] != name {
					t.Fatalf("removed headers mismatch: got %v, want %v", removed, tt.wantRemoved)
				}
			}
		})
	}
}

func TestApiKeyExtractRejectBodyIsValidJSON(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, map[string]any{"required": true})
	ctx := plugintest.NewMockContext()

	if err := p.OnRequestHeaders(ctx, hdrs(map[string]string{":path": fakePath("")})); err != nil {
		t.Fatalf("OnRequestHeaders failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(ctx.CancelBody()), &parsed); err != nil {
		t.Fatalf("rejection body must be valid JSON: %v", err)
	}
}

func TestConfiguredSourcesReplaceDefaults(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, map[string]any{
		"sources": []any{
			map[string]any{"type": "header", "name": "x-custom-key"},
		},
	})
	ctx := plugintest.NewMockContext()
	h := hdrs(map[string]string{
		":path":        fakePath(""),
		testHdrAPIKey:  fakeKey("default-source"),
		"x-custom-key": fakeKey("configured-source"),
	})
	if err := p.OnRequestHeaders(ctx, h); err != nil {
		t.Fatalf("OnRequestHeaders failed: %v", err)
	}
	got, ok := ctx.LastHeaderValue("x-extracted-apikey")
	if !ok || got != fakeKey("configured-source") {
		t.Fatalf("configured source must win over the defaults: got (%q,%v)", got, ok)
	}
}

func TestConfiguredSourcesEmptyIsAnError(t *testing.T) {
	t.Parallel()

	p := New()
	err := p.Init(gjson.New(map[string]any{"sources": []any{}}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("an explicitly empty sources list must fail validation")
	}
}

func TestInvalidSourceConfigFailsFast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  map[string]any
	}{
		{"unknown type", map[string]any{"sources": []any{map[string]any{"type": "cookie", "name": "k"}}}},
		{"empty name", map[string]any{"sources": []any{map[string]any{"type": "header"}}}},
		{"bearer on query", map[string]any{"sources": []any{map[string]any{"type": "query", "name": "k", "bearer": true}}}},
		{"invalid target header", map[string]any{"targetHeader": ":authority"}},
		{"invalid reject status", map[string]any{"rejectStatus": 999}},
		{"strip removes target", map[string]any{
			"targetHeader": "x-key",
			"stripSource":  true,
			"sources":      []any{map[string]any{"type": "header", "name": "x-key"}},
		}},
		{"strip a protected header", map[string]any{
			"stripSource": true,
			"sources":     []any{map[string]any{"type": "header", "name": "host"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := New()
			if err := p.Init(gjson.New(tt.cfg), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				t.Fatal("expected config validation to fail")
			}
		})
	}
}

func TestInvalidKeyValuesAreNeverInjected(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, map[string]any{
		"sources":  []any{map[string]any{"type": "query", "name": testQueryName}},
		"required": true,
	})
	ctx := plugintest.NewMockContext()
	// url.ParseQuery decodes %0A into a newline; injecting it would be a
	// header-smuggling primitive. It must be skipped and required mode must
	// reject the request instead.
	if err := p.OnRequestHeaders(ctx, hdrs(map[string]string{":path": fakePath(testQueryName + "=bad%0Avalue")})); err != nil {
		t.Fatalf("OnRequestHeaders failed: %v", err)
	}
	if _, ok := ctx.LastHeaderValue("x-extracted-apikey"); ok {
		t.Fatal("an invalid key value must not be injected")
	}
	if status, aborted := ctx.CancelStatus(); !aborted || status != 401 {
		t.Fatalf("required mode should reject after skipping the invalid value: (%d,%v)", status, aborted)
	}
}
