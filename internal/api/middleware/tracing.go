package middleware

import (
    "fmt"
    "net/http"
    "time"

    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
    "go.opentelemetry.io/otel/trace"
)

// # TracingMiddleware provides distributed tracing with OpenTelemetry
// # Supports W3C TraceContext and Baggage propagation
type TracingMiddleware struct {
    tracer           trace.Tracer
    meter            metric.Meter
    requestCounter   metric.Int64Counter
    requestDuration  metric.Float64Histogram
    activeRequests   metric.Int64UpDownCounter
    requestSize      metric.Int64Histogram
    responseSize     metric.Int64Histogram
}

// # NewTracingMiddleware initializes with OpenTelemetry instruments
func NewTracingMiddleware(serviceName string) *TracingMiddleware {
    tracer := otel.Tracer(
        fmt.Sprintf("%s-http", serviceName),
        trace.WithInstrumentationVersion("1.0.0"),
    )
    
    meter := otel.Meter(
        fmt.Sprintf("%s-http", serviceName),
        metric.WithInstrumentationVersion("1.0.0"),
    )

    // # Create metrics for RED method (Rate, Errors, Duration)
    requestCounter, _ := meter.Int64Counter(
        "http_requests_total",
        metric.WithDescription("Total number of HTTP requests"),
        metric.WithUnit("{request}"),
    )

    requestDuration, _ := meter.Float64Histogram(
        "http_request_duration_seconds",
        metric.WithDescription("Duration of HTTP requests"),
        metric.WithUnit("s"),
        metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
    )

    activeRequests, _ := meter.Int64UpDownCounter(
        "http_requests_active",
        metric.WithDescription("Number of active HTTP requests"),
        metric.WithUnit("{request}"),
    )

    requestSize, _ := meter.Int64Histogram(
        "http_request_size_bytes",
        metric.WithDescription("Size of HTTP requests"),
        metric.WithUnit("By"),
        metric.WithExplicitBucketBoundaries(100, 1000, 10000, 100000, 1000000),
    )

    responseSize, _ := meter.Int64Histogram(
        "http_response_size_bytes",
        metric.WithDescription("Size of HTTP responses"),
        metric.WithUnit("By"),
        metric.WithExplicitBucketBoundaries(100, 1000, 10000, 100000, 1000000),
    )

    return &TracingMiddleware{
        tracer:          tracer,
        meter:           meter,
        requestCounter:  requestCounter,
        requestDuration: requestDuration,
        activeRequests:  activeRequests,
        requestSize:     requestSize,
        responseSize:    responseSize,
    }
}

// # Trace wraps HTTP handler with distributed tracing
func (tm *TracingMiddleware) Trace(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // # Extract trace context from incoming request headers
        propagator := propagation.TraceContext{}
        ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
        
        // # Start new span for this request
        spanName := fmt.Sprintf("%s %s", r.Method, r.URL.Path)
        ctx, span := tm.tracer.Start(ctx, spanName,
            trace.WithSpanKind(trace.SpanKindServer),
            trace.WithAttributes(
                semconv.HTTPMethodKey.String(r.Method),
                semconv.HTTPURLKey.String(r.URL.String()),
                semconv.HTTPSchemeKey.String(r.URL.Scheme),
                semconv.HTTPTargetKey.String(r.URL.Path),
                semconv.HTTPUserAgentKey.String(r.UserAgent()),
                semconv.NetHostNameKey.String(r.Host),
                attribute.String("http.client_ip", r.RemoteAddr),
                attribute.String("http.request_id", 
                    r.Context().Value(ContextKeyRequestID).(string)),
            ),
        )
        defer span.End()

        // # Record request size
        if r.ContentLength > 0 {
            tm.requestSize.Record(ctx, r.ContentLength)
            span.SetAttributes(attribute.Int64("http.request_content_length", r.ContentLength))
        }

        // # Track active requests
        tm.activeRequests.Add(ctx, 1)
        defer tm.activeRequests.Add(ctx, -1)

        // # Create response wrapper to capture status code and response size
        wrapped := &responseWriterWrapper{
            ResponseWriter: w,
            statusCode:     http.StatusOK,
            bytesWritten:   0,
        }

        // # Measure request duration
        startTime := time.Now()
        
        // # Add trace ID to response headers for debugging
        spanContext := span.SpanContext()
        if spanContext.HasTraceID() {
            w.Header().Set("X-Trace-ID", spanContext.TraceID().String())
        }
        if spanContext.HasSpanID() {
            w.Header().Set("X-Span-ID", spanContext.SpanID().String())
        }

        // # Process request
        next.ServeHTTP(wrapped, r.WithContext(ctx))

        // # Record metrics after request completion
        duration := time.Since(startTime)
        statusCode := wrapped.statusCode

        // # Record span attributes
        span.SetAttributes(
            semconv.HTTPStatusCodeKey.Int(statusCode),
            attribute.Int64("http.response_size", wrapped.bytesWritten),
            attribute.Float64("http.duration_ms", float64(duration.Milliseconds())),
        )

        // # Record span status based on HTTP status code
        if statusCode >= 400 {
            span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
            if statusCode >= 500 {
                span.RecordError(fmt.Errorf("server error: HTTP %d", statusCode))
            }
        } else {
            span.SetStatus(codes.Ok, "")
        }

        // # Record RED metrics
        tm.requestCounter.Add(ctx, 1,
            metric.WithAttributes(
                attribute.String("http.method", r.Method),
                attribute.String("http.target", r.URL.Path),
                attribute.Int("http.status_code", statusCode),
            ),
        )

        tm.requestDuration.Record(ctx, duration.Seconds(),
            metric.WithAttributes(
                attribute.String("http.method", r.Method),
                attribute.String("http.target", r.URL.Path),
            ),
        )

        if wrapped.bytesWritten > 0 {
            tm.responseSize.Record(ctx, wrapped.bytesWritten,
                metric.WithAttributes(
                    attribute.Int("http.status_code", statusCode),
                ),
            )
        }

        // # Log request details for debugging
        log.Debug().
            Str("method", r.Method).
            Str("path", r.URL.Path).
            Int("status", statusCode).
            Dur("duration", duration).
            Int64("response_size", wrapped.bytesWritten).
            Str("trace_id", spanContext.TraceID().String()).
            Msg("HTTP request completed")
    })
}

// # responseWriterWrapper captures response metadata for observability
type responseWriterWrapper struct {
    http.ResponseWriter
    statusCode   int
    bytesWritten int64
}

// # WriteHeader captures the status code
func (rw *responseWriterWrapper) WriteHeader(code int) {
    rw.statusCode = code
    rw.ResponseWriter.WriteHeader(code)
}

// # Write captures the response size
func (rw *responseWriterWrapper) Write(b []byte) (int, error) {
    n, err := rw.ResponseWriter.Write(b)
    rw.bytesWritten += int64(n)
    return n, err
}

// # Flush implements http.Flusher for streaming responses
func (rw *responseWriterWrapper) Flush() {
    if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
        flusher.Flush()
    }
}

// # Hijack implements http.Hijacker for WebSocket upgrades
func (rw *responseWriterWrapper) Hijack() (net.Conn, *bufio.ReadWriter, error) {
    if hijacker, ok := rw.ResponseWriter.(http.Hijacker); ok {
        return hijacker.Hijack()
    }
    return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
}
