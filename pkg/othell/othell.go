package othell

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"cloud.google.com/go/compute/metadata"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Othell struct {
	CollectorEndpoint string
	TraceProvider     *sdktrace.TracerProvider
	MeterProvider     *sdkmetric.MeterProvider
	Tracer            trace.Tracer
	Meter             metric.Meter
	Resource          *resource.Resource
	DebugTracer       bool
}

type Option func(*Othell)

var projectID string // Global variable to store the project ID

func init() {
	projectID = getProjectID() // Fetch project ID once during startup
}

func getProjectID() string {
	if metadata.OnGCE() {
		if id, err := metadata.ProjectIDWithContext(context.Background()); err == nil {
			return id
		}
	}
	return "non-gcp" // Fallback if not running on GCP
}

// New creates a new Othell instance with the provided options.
// The name is important for disambiguating the service or module.
func New(name string, opts ...Option) (*Othell, error) {
	o := &Othell{}
	for _, opt := range opts {
		opt(o)
	}

	if o.Resource == nil {
		return nil, fmt.Errorf("resource is required. Use WithResoure()")
	}

	if err := o.initTracer(); err != nil {
		return o, err
	}

	if err := o.initMeter(); err != nil {
		return o, err
	}

	if err := o.initLogging(); err != nil {
		return o, err
	}

	o.Meter = otel.Meter(name + "-meter")
	o.Tracer = otel.Tracer(name + "-tracer")
	return o, nil
}

func WithCollectorEndpoint(endpoint string) Option {
	return func(o *Othell) {
		o.CollectorEndpoint = endpoint
	}
}

func WithResource(res *resource.Resource) Option {
	return func(o *Othell) {
		o.Resource = res
	}
}

func WithDebugTracer() Option {
	return func(o *Othell) {
		o.DebugTracer = true
	}
}

func (o *Othell) initTracer() error {
	ctx := context.Background()

	// Set global propagators (W3C and baggage).
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator())

	otelExporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return err
	}

	var consoleExporter *stdouttrace.Exporter

	traceProviderOptions := []sdktrace.TracerProviderOption{}
	traceProviderOptions = append(traceProviderOptions, sdktrace.WithSampler(sdktrace.AlwaysSample()))
	traceProviderOptions = append(traceProviderOptions, sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(otelExporter)))
	traceProviderOptions = append(traceProviderOptions, sdktrace.WithResource(o.Resource))
	if o.DebugTracer {
		var err error
		consoleExporter, err = stdouttrace.New(
			stdouttrace.WithPrettyPrint(),
		)
		if err != nil {
			return fmt.Errorf("failed to create console exporter: %w", err)
		}

		traceProviderOptions = append(traceProviderOptions, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(consoleExporter)))
	}

	o.TraceProvider = sdktrace.NewTracerProvider(
		traceProviderOptions...,
	)

	// Set the global provider so otel.Tracer(...) picks it up.
	otel.SetTracerProvider(o.TraceProvider)

	return nil
}

func (o *Othell) initMeter() error {
	ctx := context.Background()

	meter, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return errors.Join(err)
	}

	o.MeterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(meter),
		sdkmetric.WithResource(o.Resource),
	)

	otel.SetMeterProvider(o.MeterProvider)
	return nil
}

func (o *Othell) initLogging() error {
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: replacer})
	instrumentedHandler := handlerWithSpanContext(jsonHandler)
	slog.SetDefault(slog.New(instrumentedHandler))
	return nil
}

// This code is lifted from Google documentation

func handlerWithSpanContext(handler slog.Handler) *spanContextLogHandler {
	return &spanContextLogHandler{Handler: handler}
}

// spanContextLogHandler is a slog.Handler which adds attributes from the
// span context.
type spanContextLogHandler struct {
	slog.Handler
}

// Handle overrides slog.Handler's Handle method. This adds attributes from the
// span context to the slog.Record.
func (t *spanContextLogHandler) Handle(ctx context.Context, record slog.Record) error {
	// Get the SpanContext from the context.
	if s := trace.SpanContextFromContext(ctx); s.IsValid() {
		// Add trace context attributes following Cloud Logging structured log format described
		// in https://cloud.google.com/logging/docs/structured-logging#special-payload-fields
		record.AddAttrs(
			slog.Any("logging.googleapis.com/trace", fmt.Sprintf("projects/%s/traces/%s", projectID, s.TraceID())),
		)
		record.AddAttrs(
			slog.Any("logging.googleapis.com/spanId", s.SpanID()),
		)
		record.AddAttrs(
			slog.Bool("logging.googleapis.com/trace_sampled", s.TraceFlags().IsSampled()),
		)
	}
	return t.Handler.Handle(ctx, record)
}

func replacer(groups []string, a slog.Attr) slog.Attr {
	// Rename attribute keys to match Cloud Logging structured log format
	switch a.Key {
	case slog.LevelKey:
		a.Key = "severity"
		// Map slog.Level string values to Cloud Logging LogSeverity
		// https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#LogSeverity
		if level := a.Value.Any().(slog.Level); level == slog.LevelWarn {
			a.Value = slog.StringValue("WARNING")
		}
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}
