package extproc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// RequestProcessor is the core interface implemented by plugins/processors:
// six phase callbacks + error handling + lifecycle. GenericExtProcServer owns
// the stream I/O and metrics; implementations write business logic only.
type RequestProcessor interface {
	GetName() string
	GetOptions() *ProcessingOptions

	// Request/response headers.
	ProcessRequestHeaders(ctx *RequestContext, headers AllHeaders) error
	ProcessResponseHeaders(ctx *RequestContext, headers AllHeaders) error

	// Bodies (or body chunks). Under a STREAMED body mode data is not buffered,
	// so chunks can be partial.
	ProcessResponseBody(ctx *RequestContext, body []byte) error
	ProcessRequestBody(ctx *RequestContext, body []byte) error

	// Request/response trailers.
	ProcessRequestTrailers(ctx *RequestContext, trailers AllHeaders) error
	ProcessResponseTrailers(ctx *RequestContext, trailers AllHeaders) error

	// ErrorHandler is called when the SDK hits an internal error; the
	// implementation may react however it likes. phase is one of the phase
	// constants in this package.
	ErrorHandler(ctx *RequestContext, phase int, err error)

	// Close notifies the processor of shutdown; gracePeriodSeconds is the
	// drain window in seconds.
	Close(gracePeriodSeconds int32) error
}

// GenericExtProcServer is the generic ext_proc service: one goroutine per
// stream, sequential Recv -> processPhase -> Send, with metrics, an empty-
// response fallback and panic recovery built in.
type GenericExtProcServer struct {
	name      string
	processor RequestProcessor
	options   *ProcessingOptions
	metrics   *Metrics
	logger    *slog.Logger
}

func (s *GenericExtProcServer) Close(gracePeriodSeconds int32) error {
	return s.processor.Close(gracePeriodSeconds)
}

// Process implements the streaming RPC of extprocv3.ExternalProcessorServer.
// Every processor callback in the loop is panic-protected: a panic terminates
// only the current stream (Envoy applies its failure_mode_allow policy) instead
// of crashing the whole process.
func (s *GenericExtProcServer) Process(srv extprocv3.ExternalProcessor_ProcessServer) error {
	if s.processor == nil {
		msg := "cannot process request stream without `processor` interface"
		s.logger.Error(msg)
		return errors.New(msg)
	}

	s.logger.Debug("Starting request stream", slog.String("name", s.name))

	rc := newRequestContext()
	ctx := srv.Context()
	rc.setContext(ctx)
	if lifecycle, ok := s.processor.(StreamLifecycle); ok {
		lifecycle.StreamStart(rc)
		defer lifecycle.StreamEnd(rc)
	}

	// A stream is counted as errored at most once; PhaseErrors carries the
	// per-phase detail.
	errored := false

	s.metrics.TotalStreams.Inc()

	s.metrics.ActiveStreams.Inc()
	defer func() {
		s.metrics.ActiveStreams.Dec()
		if errored {
			s.metrics.ErroredStreams.Inc()
		}
	}()

	ss := time.Now()
	defer func() {
		dur := time.Since(ss).Seconds()
		s.metrics.StreamDurationSeconds.Observe(float64(dur))
	}()

	for {
		select {
		case <-ctx.Done():
			s.logger.Debug("Request stream terminated", slog.String("name", s.name))
			return ctx.Err()

		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				// Client cancellation is a normal path — not an error.
				s.logger.Debug("Processing stream cancelled", slog.String("error", err.Error()))
				return nil
			}
			errored = true
			s.notifyError(rc, REQUEST_PHASE_UNDETERMINED, err)
			return status.Errorf(codes.Unknown, "failed to receive stream request: %v", err)
		}

		// Clear the previous phase's response state; each phase has its own
		// response shape.
		err = rc.ResetPhase()
		if err != nil {
			errored = true
			s.metrics.PhaseErrors.Inc()
			s.logger.Error("Phase processing error", slog.String("phase", RequestPhaseToString(REQUEST_PHASE_UNDETERMINED)), slog.String("error", err.Error()))
			if s.options.AbortOnProcessorFailure {
				return status.Errorf(codes.Unknown, "failed to reset extproc context: %v", err)
			}
		}

		resp, phase, err := s.safeProcessPhase(req, s.processor, rc)
		phaseName := RequestPhaseToString(phase)
		if err != nil {
			errored = true
			s.metrics.PhaseErrors.Inc()
			s.logger.Error("Phase processing error", slog.String("phase", phaseName), slog.String("error", err.Error()))
			s.notifyError(rc, phase, err)
			if s.options.AbortOnProcessorFailure {
				return status.Errorf(codes.Unknown, "processor failed and abort requested: %v", err)
			}
		}
		if resp == nil {
			// The processor set no response: fall back to an empty continue and
			// count it — never leave the stream hanging until timeout.
			s.metrics.TotalEmptyResponses.Inc()
			s.logger.Warn("Phase processing did not define a response", slog.String("phase", RequestPhaseToString(phase)))
			resp, err = rc.EmptyContinueResponse(phase)
			if err != nil {
				errored = true
				s.logger.Error("Failed to construct empty continue response for phase", slog.String("phase", phaseName), slog.String("error", err.Error()))
				return status.Errorf(codes.Unknown, "failed to construct empty continue response: %v", err)
			}
		}

		s.logPhase(rc, phaseName)
		if err := srv.Send(resp); err != nil {
			errored = true
			s.metrics.ResponseSendErrors.Inc()
			s.logger.Error("Send ProcessingResponse error", slog.String("phase", phaseName), slog.String("error", err.Error()))
			s.notifyError(rc, phase, err)
			return status.Errorf(codes.Unknown, "failed to send response to envoy: %v", err)
		}

	} // end of the stream message loop
}

// safeProcessPhase runs a single phase with panic protection.
// The original SDK lacked this guard: a processor panic propagated through the
// gRPC handler and crashed the whole process, taking every in-flight stream in
// the pod with it. After recovery we return an error so Envoy applies its
// failure policy to the current stream.
func (s *GenericExtProcServer) safeProcessPhase(procReq *extprocv3.ProcessingRequest, processor RequestProcessor, rc *RequestContext) (resp *extprocv3.ProcessingResponse, phase int, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.metrics.PanicRecoveries.Inc()
			s.logger.Error("processor panic recovered (current stream follows the failure policy)",
				slog.String("processor", s.name),
				slog.String("panic", fmt.Sprint(r)))
			resp, phase, err = nil, rc.GetPhase(), fmt.Errorf("processor panic: %v", r)
		}
	}()
	return s.processPhase(procReq, processor, rc)
}

// notifyError forwards internal errors to the processor callback (the callback
// itself may panic — guarded as well).
func (s *GenericExtProcServer) notifyError(rc *RequestContext, phase int, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.metrics.PanicRecoveries.Inc()
			s.logger.Error("ErrorHandler panic recovered", slog.String("panic", fmt.Sprint(r)))
		}
	}()
	s.processor.ErrorHandler(rc, phase, err)
}

// logPhase emits metadata-only debug logging for a phase. Raw headers and
// bodies are deliberately never logged: they can contain Authorization values,
// API keys, cookies and user payloads, and debug logs are often shipped to a
// central collector.
func (s *GenericExtProcServer) logPhase(rc *RequestContext, phase string, attrs ...slog.Attr) {
	if s.logger == nil || !s.logger.Enabled(rc.Context(), slog.LevelDebug) {
		return
	}
	base := []slog.Attr{
		slog.String("phase", phase),
		slog.String("requestId", rc.RequestID),
		slog.String("traceId", rc.TraceID),
		slog.String("authority", rc.Authority),
		slog.String("method", rc.Method),
		slog.String("path", rc.Path),
	}
	s.logger.LogAttrs(rc.Context(), slog.LevelDebug, "processing phase", append(base, attrs...)...)
}

// processPhase dispatches one phase: routes the message type to the matching
// processor callback and accumulates metrics and duration.
func (s *GenericExtProcServer) processPhase(procReq *extprocv3.ProcessingRequest, processor RequestProcessor, rc *RequestContext) (*extprocv3.ProcessingResponse, int, error) {
	if rc == nil {
		return nil, REQUEST_PHASE_UNDETERMINED, errors.New("request context is nil")
	}

	var (
		ps  time.Time
		dur time.Duration
		err error
	)

	phase := REQUEST_PHASE_UNDETERMINED

	ps = time.Now()
	switch req := procReq.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		phase = REQUEST_PHASE_REQUEST_HEADERS
		rc.setPhase(phase)
		s.logPhase(rc, "REQUEST_HEADERS")
		s.metrics.TotalRequestHeaders.Inc()

		h := req.RequestHeaders

		// Initialize the request context (requires Envoy NOT to configure
		// request_header_mode: SKIP, otherwise this is never reached).
		err = initReqCtx(rc, h.Headers)
		if err != nil {
			s.logger.Error("RequestContext initialization error", slog.String("error", err.Error()))
			return nil, phase, err
		}
		rc.EndOfStream = h.EndOfStream

		err = processor.ProcessRequestHeaders(rc, rc.AllHeaders) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

	case *extprocv3.ProcessingRequest_RequestBody:
		phase = REQUEST_PHASE_REQUEST_BODY
		rc.setPhase(phase)
		s.logPhase(rc, "REQUEST_BODY", slog.Int("bodyBytes", len(req.RequestBody.Body)), slog.Bool("endOfStream", req.RequestBody.EndOfStream))
		s.metrics.TotalRequestBody.Inc()

		b := req.RequestBody
		s.metrics.BodyBytesReceived.Add(float64(len(b.Body)))

		rc.EndOfStream = b.EndOfStream

		err = processor.ProcessRequestBody(rc, b.Body) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

	case *extprocv3.ProcessingRequest_RequestTrailers:
		phase = REQUEST_PHASE_REQUEST_TRAILERS
		rc.setPhase(phase)
		s.logPhase(rc, "REQUEST_TRAILERS")
		s.metrics.TotalRequestTrailers.Inc()

		ts := req.RequestTrailers

		var trailers AllHeaders
		trailers, err = genHeaders(ts.Trailers)
		if err != nil {
			s.logger.Error("Failed to generate request trailers", slog.String("error", err.Error()))
			return nil, phase, err
		}

		err = processor.ProcessRequestTrailers(rc, trailers) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		phase = REQUEST_PHASE_RESPONSE_HEADERS
		rc.setPhase(phase)
		s.logPhase(rc, "RESPONSE_HEADERS")
		s.metrics.TotalResponseHeaders.Inc()

		hs := req.ResponseHeaders
		rc.EndOfStream = hs.EndOfStream

		var headers AllHeaders
		headers, err = genHeaders(hs.Headers)
		if err != nil {
			s.logger.Error("Failed to generate response headers", slog.String("error", err.Error()))
			return nil, phase, err
		}

		err = processor.ProcessResponseHeaders(rc, headers) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

		if s.options.UpdateExtProcHeader {
			rc.AppendHeader("x-extproc-names", HeaderValue{RawValue: []byte(s.name)})
		}
		if rc.EndOfStream && s.options.UpdateDurationHeader {
			rc.AppendHeader("x-extproc-duration-ns", HeaderValue{RawValue: []byte(strconv.FormatInt(rc.Duration.Nanoseconds(), 10))})
		}

	case *extprocv3.ProcessingRequest_ResponseBody:
		phase = REQUEST_PHASE_RESPONSE_BODY
		rc.setPhase(phase)
		s.logPhase(rc, "RESPONSE_BODY", slog.Int("bodyBytes", len(req.ResponseBody.Body)), slog.Bool("endOfStream", req.ResponseBody.EndOfStream))
		s.metrics.TotalResponseBody.Inc()

		b := req.ResponseBody
		rc.EndOfStream = b.EndOfStream

		s.metrics.BodyBytesReturned.Add(float64(len(b.Body)))

		err = processor.ProcessResponseBody(rc, b.Body) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

		if rc.EndOfStream && s.options.UpdateDurationHeader {
			rc.AppendHeader("x-extproc-duration-ns", HeaderValue{RawValue: []byte(strconv.FormatInt(rc.Duration.Nanoseconds(), 10))})
		}

	case *extprocv3.ProcessingRequest_ResponseTrailers:
		phase = REQUEST_PHASE_RESPONSE_TRAILERS
		rc.setPhase(phase)
		s.logPhase(rc, "RESPONSE_TRAILERS")
		s.metrics.TotalResponseTrailers.Inc()

		ts := req.ResponseTrailers

		var trailers AllHeaders
		trailers, err = genHeaders(ts.Trailers)
		if err != nil {
			s.logger.Error("Failed to generate response trailers", slog.String("error", err.Error()))
			return nil, phase, err
		}

		err = processor.ProcessResponseTrailers(rc, trailers) //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur

	default:
		s.logger.Warn("Unknown request type", slog.String("requestType", fmt.Sprintf("%T", req)))
		err = errors.New("unknown request type") //nolint:errcheck,staticcheck
		dur = time.Since(ps)
		rc.Duration += dur
	}

	if err != nil {
		return nil, phase, err
	}

	phaseName := RequestPhaseToString(phase)
	s.metrics.PhaseDurationSeconds.WithLabelValues(phaseName).Observe(dur.Seconds())
	s.logPhase(rc, phaseName, slog.Duration("duration", dur))
	response, err := rc.GetResponse(phase)
	return response, phase, err
}
