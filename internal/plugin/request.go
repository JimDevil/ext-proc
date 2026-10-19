package plugin

// RequestInfo is the request metadata captured from the request-headers
// phase. Body/trailer phases cannot read headers, so the bridge keeps this
// available for the whole stream.
type RequestInfo struct {
	Scheme    string
	Authority string
	Method    string
	Path      string
	FullPath  string
	RequestID string
	TraceID   string
	SpanID    string
}
