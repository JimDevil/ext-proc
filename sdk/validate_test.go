package extproc

import (
	"context"
	"testing"
)

func TestValidateHeaderName(t *testing.T) {
	t.Parallel()

	valid := []string{"x-api-key", "authorization", "x-higress-llm-model", "a.b-c_d"}
	for _, name := range valid {
		if err := ValidateHeaderName(name); err != nil {
			t.Fatalf("ValidateHeaderName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", ":authority", "bad header", "bad\nname", "bad:name"}
	for _, name := range invalid {
		if err := ValidateHeaderName(name); err == nil {
			t.Fatalf("ValidateHeaderName(%q) = nil, want an error", name)
		}
	}
}

func TestValidateHeaderValue(t *testing.T) {
	t.Parallel()

	valid := [][]byte{[]byte("plain"), []byte("with\ttab"), []byte("utf8-\xc3\xa9"), nil}
	for _, value := range valid {
		if err := ValidateHeaderValue(value); err != nil {
			t.Fatalf("ValidateHeaderValue(%q) = %v, want nil", value, err)
		}
	}
	invalid := [][]byte{[]byte("bad\rvalue"), []byte("bad\nvalue"), []byte("bad\x00value"), []byte("bad\x7fvalue")}
	for _, value := range invalid {
		if err := ValidateHeaderValue(value); err == nil {
			t.Fatalf("ValidateHeaderValue(%q) = nil, want an error", value)
		}
	}
}

func TestValidateStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int32{100, 200, 401, 503, 599} {
		if err := ValidateStatus(status); err != nil {
			t.Fatalf("ValidateStatus(%d) = %v, want nil", status, err)
		}
	}
	for _, status := range []int32{0, 99, 600, 999} {
		if err := ValidateStatus(status); err == nil {
			t.Fatalf("ValidateStatus(%d) = nil, want an error", status)
		}
	}
}

func TestMutableHeaderValidation(t *testing.T) {
	t.Parallel()

	rc := NewRequestContext()
	if err := rc.OverwriteHeader("host", HeaderValue{Value: "x"}); err == nil {
		t.Fatal("host must be rejected as an Envoy-protected header")
	}
	if err := rc.OverwriteHeader("x-envoy-upstream-rq-timeout-ms", HeaderValue{Value: "1"}); err == nil {
		t.Fatal("x-envoy-* must be rejected")
	}
	if err := rc.OverwriteHeader(":authority", HeaderValue{Value: "x"}); err == nil {
		t.Fatal("pseudo headers must be rejected")
	}
	if err := rc.OverwriteHeader("x-ok", HeaderValue{Value: "bad\r\nvalue"}); err == nil {
		t.Fatal("values with CR/LF must be rejected")
	}
	if err := rc.OverwriteHeader("x-ok", HeaderValue{Value: "ok"}); err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}
	if err := rc.RemoveHeader("host"); err == nil {
		t.Fatal("removing a protected header must be rejected")
	}
}

func TestCancelRequestValidatesStatus(t *testing.T) {
	t.Parallel()

	rc := NewRequestContext()
	if err := rc.CancelRequest(999, nil, "nope"); err == nil {
		t.Fatal("invalid status must be rejected")
	}
	if err := rc.CancelRequest(401, map[string]HeaderValue{"content-type": {Value: "application/json"}}, "{}"); err != nil {
		t.Fatalf("valid immediate response rejected: %v", err)
	}
}

func TestReplaceBodyDoesNotTouchContentLength(t *testing.T) {
	t.Parallel()

	rc := NewRequestContext()
	if err := rc.ResetPhase(); err != nil {
		t.Fatal(err)
	}
	if err := rc.ReplaceBody([]byte(`{"model":"x"}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := rc.GetResponse(REQUEST_PHASE_REQUEST_BODY)
	if err != nil {
		t.Fatal(err)
	}
	bodyResp := resp.GetRequestBody()
	if bodyResp == nil || bodyResp.GetResponse() == nil {
		t.Fatal("missing body response")
	}
	if got := bodyResp.GetResponse().GetHeaderMutation().GetSetHeaders(); len(got) != 0 {
		t.Fatalf("ReplaceBody must leave Content-Length to Envoy, got %d header mutations", len(got))
	}
}

func TestPhaseSnapshotRestore(t *testing.T) {
	t.Parallel()

	rc := NewRequestContext()
	if err := rc.ResetPhase(); err != nil {
		t.Fatal(err)
	}
	if err := rc.OverwriteHeader("x-before", HeaderValue{Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := rc.SetValue("before", "1"); err != nil {
		t.Fatal(err)
	}
	snapshot := rc.SnapshotPhase()

	if err := rc.SetValue("after", "2"); err != nil {
		t.Fatal(err)
	}
	if err := rc.OverwriteHeader("x-after", HeaderValue{Value: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := rc.ReplaceBody([]byte("changed")); err != nil {
		t.Fatal(err)
	}
	rc.ClearRouteCache()
	if err := rc.CancelRequest(403, map[string]HeaderValue{"x-immediate": {Value: "1"}}, "denied"); err != nil {
		t.Fatal(err)
	}

	rc.RestorePhase(snapshot)
	resp, err := rc.GetResponse(REQUEST_PHASE_REQUEST_HEADERS)
	if err != nil {
		t.Fatal(err)
	}
	common := resp.GetRequestHeaders().GetResponse()
	if resp.GetImmediateResponse() != nil {
		t.Fatal("immediate response must be rolled back")
	}
	if common.GetClearRouteCache() {
		t.Fatal("clear_route_cache must be rolled back")
	}
	headers := common.GetHeaderMutation().GetSetHeaders()
	if len(headers) != 1 || headers[0].GetHeader().GetKey() != "x-before" {
		t.Fatalf("header mutation rollback mismatch: %v", headers)
	}
	if common.GetBodyMutation() != nil {
		t.Fatal("body mutation must be rolled back")
	}
	if _, err := rc.GetValue("after"); err == nil {
		t.Fatal("request-scoped state written after the snapshot must be rolled back")
	}
	if v, err := rc.GetValue("before"); err != nil || v != "1" {
		t.Fatalf("request-scoped state from the snapshot must survive: (%v,%v)", v, err)
	}
}

func TestRequestContextContext(t *testing.T) {
	t.Parallel()

	rc := NewRequestContext()
	if rc.Context() == nil {
		t.Fatal("Context() must never be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc.setContext(ctx)
	if rc.Context() != ctx {
		t.Fatal("Context() must return the stream context")
	}
	cancel()
	if rc.Context().Err() == nil {
		t.Fatal("cancellation must be observable by plugins")
	}
}
