package dynamic

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/plugin/plugintest"
	"github.com/JimDevil/ext-proc/internal/pluginhost"
	extproc "github.com/JimDevil/ext-proc/sdk"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// wireStub is served in-process so adapter tests exercise the full
// host-side translation (ProcessingRequest out, mutations applied in)
// without spawning a binary.
type wireStub struct {
	plugin.BasePlugin
	value  string
	reject int32
}

func (p *wireStub) Name() string { return "stub" }

func (p *wireStub) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	if p.reject != 0 {
		return ctx.CancelRequest(p.reject, nil, "denied")
	}
	return ctx.OverwriteHeader("x-mutated", extproc.HeaderValue{Value: p.value})
}

func (p *wireStub) OnRequestBody(ctx plugin.Context, body []byte) error {
	return ctx.ReplaceBody([]byte("replaced:" + string(body)))
}

// serveStub serves the stub and returns a managedProcess bound to its
// connection (bypassing the exec path — that is covered by process_test).
func serveStub(t *testing.T, p plugin.Plugin) (*managedProcess, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := extproc.AssembleExtProcServer(extproc.NewDefaultServerOptions(), pluginhost.NewSingleProcessor("stub", p, testLog), testLog)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	mp := &managedProcess{name: "stub", logger: testLog, conn: conn}
	return mp, func() {
		conn.Close()
		srv.Stop()
	}
}

// newAdapter builds an adapter over a stub server.
func newAdapter(t *testing.T, p plugin.Plugin) (*subprocessPlugin, *plugintest.MockContext, func()) {
	t.Helper()
	mp, stop := serveStub(t, p)
	return &subprocessPlugin{name: "stub", proc: mp, timeout: 2 * time.Second, logger: testLog},
		plugintest.NewMockContext(), stop
}

// allHeaders builds an AllHeaders with one header per pair.
func allHeaders(pairs ...string) extproc.AllHeaders {
	h := extproc.AllHeaders{
		Headers:    map[string][]string{},
		RawHeaders: map[string][][]byte{},
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Headers[pairs[i]] = append(h.Headers[pairs[i]], pairs[i+1])
		h.RawHeaders[pairs[i]] = append(h.RawHeaders[pairs[i]], []byte(pairs[i+1]))
	}
	return h
}

func TestAdapterAppliesMutations(t *testing.T) {
	t.Parallel()
	sp, mctx, stop := newAdapter(t, &wireStub{value: "on"})
	defer stop()

	if err := sp.OnRequestHeaders(mctx, allHeaders("x-old", "1")); err != nil {
		t.Fatalf("OnRequestHeaders: %v", err)
	}
	if v, ok := mctx.LastHeaderValue("x-mutated"); !ok || v != "on" {
		t.Fatalf("x-mutated not applied: %v %v", v, ok)
	}

	// Body replacement flows through ReplaceBody.
	bctx := plugintest.NewMockContext()
	if err := sp.OnRequestBody(bctx, []byte("abc")); err != nil {
		t.Fatalf("OnRequestBody: %v", err)
	}
	if body, ok := bctx.Body(); !ok || string(body) != "replaced:abc" {
		t.Fatalf("body not replaced: %q %v", body, ok)
	}
}

func TestAdapterMapsImmediateResponse(t *testing.T) {
	t.Parallel()
	sp, mctx, stop := newAdapter(t, &wireStub{reject: 403})
	defer stop()

	if err := sp.OnRequestHeaders(mctx, allHeaders(":path", "/x")); err != nil {
		t.Fatalf("OnRequestHeaders: %v", err)
	}
	if status, ok := mctx.CancelStatus(); !ok || status != 403 {
		t.Fatalf("short-circuit not mapped: %v %v", status, ok)
	}
	if !mctx.Aborted() {
		t.Fatal("context should be aborted after an immediate response")
	}
}

func TestAdapterCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	sp, _, stop := newAdapter(t, &wireStub{value: "on"})
	defer stop()

	if err := sp.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The adapter never talks to a nil response set: an empty continue must be a
// no-op, not an error (guards the default branch of applyResponse).
func TestAdapterEmptyContinue(t *testing.T) {
	t.Parallel()
	mp, stop := serveStub(t, &wireStub{})
	defer stop()
	sp := &subprocessPlugin{name: "stub", proc: mp, timeout: 2 * time.Second, logger: testLog}

	mctx := plugintest.NewMockContext()
	req := &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestTrailers{
		RequestTrailers: &epb.HttpTrailers{},
	}}
	if err := sp.call(mctx, req); err != nil {
		t.Fatalf("trailers phase: %v", err)
	}
}
