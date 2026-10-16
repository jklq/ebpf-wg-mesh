package telemetry

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
	grpc_codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// connect.Code values are numerically aligned with gRPC codes by design.
func codeFromConnect(c connect.Code) grpc_codes.Code { return grpc_codes.Code(uint32(c)) }

// GRPCServerStatsHandler returns the otelgrpc stats handler: server spans,
// rpc.server.duration/request-size/response-size metrics, and W3C context
// extraction. Install with grpc.StatsHandler.
func GRPCServerStatsHandler() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler())
}

// GRPCClientStatsHandler is the client counterpart (context injection).
func GRPCClientStatsHandler() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler())
}

// GRPCUnaryInterceptor finalizes one wide event per unary RPC. Place it
// outermost so auth and handlers enrich the same operation.
func GRPCUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, op := StartOperation(ctx, "rpc", info.FullMethod)
		resp, err := handler(ctx, req)
		op.End(ctx, err)
		return resp, err
	}
}

// GRPCStreamInterceptor finalizes one wide event at stream end (agent Sync
// streams are long-lived: their wide event records the whole session hop,
// while per-message detail belongs on child spans/metrics).
func GRPCStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, op := StartOperation(stream.Context(), "rpc_stream", info.FullMethod)
		err := handler(srv, &wrappedStream{ServerStream: stream, ctx: ctx})
		op.End(ctx, err)
		return err
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// GRPCUnaryClientInterceptor adds client operation facts and ends the
// operation if the caller started one; standalone it only enriches.
func GRPCUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		Set(ctx, attribute.String("rpc.method", method), attribute.String("peer.service", cc.Target()))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// ConnectInterceptor finalizes wide events for Connect unary calls.
// Streaming Connect calls pass through (the HTTP wide event covers them).
func ConnectInterceptor() connect.Interceptor {
	return &connectInterceptor{}
}

type connectInterceptor struct{}

func (i *connectInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, op := StartOperation(ctx, "rpc", req.Spec().Procedure)
		op.Set(attribute.String("rpc.system", "connect"))
		resp, err := next(ctx, req)
		op.End(ctx, connectErr(err))
		return resp, err
	}
}

func (i *connectInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *connectInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func connectErr(err error) error {
	if err == nil {
		return nil
	}
	if ce := connect.CodeOf(err); ce != connect.CodeUnknown {
		return status.Error(codeFromConnect(ce), err.Error())
	}
	return err
}

// HTTPHandler wraps h with otelhttp (spans + http.server metrics) and the
// wide-event middleware. Operation names use the route pattern when the
// server uses Go 1.22+ patterns, else method+path class.
func HTTPHandler(h http.Handler, operation string) http.Handler {
	return wideHTTP(otelhttp.NewHandler(h, operation,
		// gRPC-over-HTTP requests get their spans and metrics from
		// otelgrpc; an HTTP span per RPC would double-count.
		otelhttp.WithFilter(func(r *http.Request) bool { return !isGRPC(r) }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Method + " " + r.Pattern
			}
			return r.Method + " " + operation
		}),
	))
}

func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && len(r.Header.Get("Content-Type")) >= len("application/grpc") &&
		r.Header.Get("Content-Type")[:len("application/grpc")] == "application/grpc"
}

// HTTPTransport instruments an outbound HTTP transport (spans, metrics,
// traceparent injection).
func HTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// HTTPClient returns a client whose transport is instrumented.
func HTTPClient(base *http.Client) *http.Client {
	if base == nil {
		return &http.Client{Transport: HTTPTransport(nil)}
	}
	clone := *base
	clone.Transport = HTTPTransport(base.Transport)
	return &clone
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush/Unwrap keep streaming handlers (gRPC-over-HTTP, SSE) functional.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func wideHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health/metrics scrapes are not operations; their absence keeps
		// wide-event volume proportional to real work.
		if isGRPC(r) || r.URL.Path == "/livez" || r.URL.Path == "/readyz" || r.URL.Path == "/admissionz" || r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, op := StartOperation(r.Context(), "http", httpName(r))
		op.Set(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))
		op.Set(attribute.Int("http.response.status_code", sw.status))
		var err error
		if sw.status >= 500 {
			err = errHTTPStatus(sw.status)
		}
		outcome := ""
		if sw.status >= 400 && sw.status < 500 {
			outcome = "client_error"
		}
		op.End(ctx, err, outcome)
	})
}

func httpName(r *http.Request) string {
	if r.Pattern != "" {
		return r.Method + " " + r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

type httpStatusError struct{ code int }

func (e httpStatusError) Error() string {
	return http.StatusText(e.code)
}

func errHTTPStatus(code int) error { return httpStatusError{code: code} }
