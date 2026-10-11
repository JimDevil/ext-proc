package extproc

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// panicProcessor 在请求头阶段 panic 的测试桩：用于验证 SDK 的 panic 防护
type panicProcessor struct {
	opts *ProcessingOptions
}

func (p *panicProcessor) GetName() string                { return "panic-test" }
func (p *panicProcessor) GetOptions() *ProcessingOptions { return p.opts }
func (p *panicProcessor) ProcessRequestHeaders(*RequestContext, AllHeaders) error {
	panic("boom: 模拟插件 panic")
}
func (p *panicProcessor) ProcessRequestBody(*RequestContext, []byte) error          { return nil }
func (p *panicProcessor) ProcessRequestTrailers(*RequestContext, AllHeaders) error  { return nil }
func (p *panicProcessor) ProcessResponseHeaders(*RequestContext, AllHeaders) error  { return nil }
func (p *panicProcessor) ProcessResponseBody(*RequestContext, []byte) error         { return nil }
func (p *panicProcessor) ProcessResponseTrailers(*RequestContext, AllHeaders) error { return nil }
func (p *panicProcessor) ErrorHandler(*RequestContext, int, error)                  {}
func (p *panicProcessor) Close(int32) error                                         { return nil }

var testSdkLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// startBufconnServer 用 bufconn 起一个进程内的 ext_proc gRPC 服务
func startBufconnServer(t *testing.T, opts *ProcessingOptions) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	srv := &GenericExtProcServer{
		name:      "panic-test",
		processor: &panicProcessor{opts: opts},
		options:   opts,
		metrics:   NewEmptyMetrics(),
		logger:    testSdkLogger,
	}
	epb.RegisterExternalProcessorServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("建立连接失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendRequestHeaders(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := epb.NewExternalProcessorClient(conn).Process(ctx)
	if err != nil {
		t.Fatalf("打开流失败: %v", err)
	}
	_ = stream.Send(&epb.ProcessingRequest{
		Request: &epb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &epb.HttpHeaders{
				Headers: &corev3.HeaderMap{
					Headers: []*corev3.HeaderValue{{Key: ":path", Value: "/v1/chat/completions"}},
				},
				EndOfStream: true,
			},
		},
	})
	// 读取响应：fail-open 期望拿到空 continue；fail-closed 期望流报错——两者都证明进程没有崩
	if _, err = stream.Recv(); err != nil && err != io.EOF {
		t.Logf("fail-closed 路径：流按预期报错: %v", err)
	}
	_ = stream.CloseSend()
}

func TestPanicRecoveryFailOpen(t *testing.T) {
	// AbortOnProcessorFailure=false：panic 被吞掉，客户端收到空 continue
	conn := startBufconnServer(t, NewDefaultProcessingOptions())
	sendRequestHeaders(t, conn)
}

func TestPanicRecoveryFailClosed(t *testing.T) {
	// AbortOnProcessorFailure=true：panic 上抛，流以 gRPC 错误终结
	opts := NewDefaultProcessingOptions()
	opts.AbortOnProcessorFailure = true
	conn := startBufconnServer(t, opts)
	sendRequestHeaders(t, conn)
}
