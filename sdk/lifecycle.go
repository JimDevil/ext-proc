package extproc

// StreamLifecycle is an optional extension implemented by a RequestProcessor
// that needs per-stream setup and teardown. The stream loop calls StreamStart
// before processing the first message and StreamEnd exactly once when the
// stream finishes (including cancellation and panic paths), which lets the
// processor release per-generation resources safely.
//
// Implementations must be cheap and must not block: both calls run on the
// stream goroutine, and StreamEnd runs in a defer.
type StreamLifecycle interface {
	StreamStart(rc *RequestContext)
	StreamEnd(rc *RequestContext)
}
