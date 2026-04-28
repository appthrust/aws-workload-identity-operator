// Package logging configures structured logging and OpenTelemetry log export.
package logging

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Exported defaults and option values used by the manager flags.
const (
	DefaultServiceName      = "aws-workload-identity-operator"
	DefaultServiceNamespace = "appthrust"
	DefaultExporter         = ExporterOTLP
	DefaultLevel            = LevelInfo

	ExporterOTLP    = "otlp"
	ExporterConsole = "console"
	ExporterNone    = "none"

	LevelTrace = "trace"
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"

	serviceNameAttr      = "service.name"
	serviceNamespaceAttr = "service.namespace"
)

// Options configures the operator logger.
type Options struct {
	ServiceName               string
	ServiceNamespace          string
	ServiceVersion            string
	DeploymentEnvironmentName string
	ResourceAttributesRaw     string
	ResourceAttributes        map[string]string
	Level                     string
	AddSource                 bool
	Exporter                  string
	InstrumentationName       string
	InstrumentationVersion    string
	ConsoleWriter             io.Writer
}

// ShutdownFunc flushes and shuts down logging resources.
type ShutdownFunc func(context.Context) error

// NewLogger builds the OpenTelemetry-backed logr logger used by controllers.
func NewLogger(ctx context.Context, opts *Options) (*sdklog.LoggerProvider, logr.Logger, ShutdownFunc, error) {
	if opts == nil {
		opts = &Options{}
	}

	level, err := parseLevel(cmp.Or(opts.Level, DefaultLevel))
	if err != nil {
		return nil, logr.Logger{}, nil, err
	}

	exporterName := resolveExporter(opts.Exporter)

	res, err := buildResource(ctx, opts)
	if err != nil {
		return nil, logr.Logger{}, nil, err
	}

	provider, shutdown, err := newProvider(ctx, exporterName, res, opts.ConsoleWriter)
	if err != nil {
		return nil, logr.Logger{}, nil, err
	}

	handler := otelslog.NewHandler(
		instrumentationName(opts),
		otelslog.WithLoggerProvider(provider),
		otelslog.WithVersion(opts.InstrumentationVersion),
		otelslog.WithSource(opts.AddSource),
	)
	handlerWithLevel := levelHandler{handler: handler, min: level}

	return provider, logr.FromSlogHandler(handlerWithLevel), shutdown, nil
}

func resolveExporter(explicit string) string {
	if explicit != "" {
		return strings.ToLower(strings.TrimSpace(explicit))
	}

	if env := os.Getenv("OTEL_LOGS_EXPORTER"); env != "" {
		return strings.ToLower(strings.TrimSpace(env))
	}

	return DefaultExporter
}

func newProvider(ctx context.Context, exporterName string, res *resource.Resource, consoleWriter io.Writer) (*sdklog.LoggerProvider, ShutdownFunc, error) {
	options := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}

	switch exporterName {
	case ExporterNone:
		provider := sdklog.NewLoggerProvider(options...)

		return provider, provider.Shutdown, nil
	case ExporterConsole:
		stdoutOptions := []stdoutlog.Option{}
		if consoleWriter != nil {
			stdoutOptions = append(stdoutOptions, stdoutlog.WithWriter(consoleWriter))
		}

		exporter, err := stdoutlog.New(stdoutOptions...)
		if err != nil {
			return nil, nil, fmt.Errorf("create stdout log exporter: %w", err)
		}

		options = append(options, sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	case ExporterOTLP:
		exporter, err := newOTLPExporter(ctx)
		if err != nil {
			return nil, nil, err
		}

		options = append(options, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	default:
		return nil, nil, fmt.Errorf("unsupported log exporter %q", exporterName)
	}

	provider := sdklog.NewLoggerProvider(options...)

	return provider, provider.Shutdown, nil
}

func newOTLPExporter(ctx context.Context) (sdklog.Exporter, error) {
	protocol := strings.ToLower(strings.TrimSpace(cmp.Or(os.Getenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"), os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"), "http/protobuf")))
	switch protocol {
	case "http/protobuf":
		exporter, err := otlploghttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP HTTP log exporter: %w", err)
		}

		return exporter, nil
	case "grpc":
		exporter, err := otlploggrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP gRPC log exporter: %w", err)
		}

		return exporter, nil
	default:
		return nil, fmt.Errorf("unsupported OTLP log protocol %q", protocol)
	}
}

func buildResource(ctx context.Context, opts *Options) (*resource.Resource, error) {
	explicit, err := explicitResourceAttributes(opts)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String(serviceNameAttr, DefaultServiceName),
			attribute.String(serviceNamespaceAttr, DefaultServiceNamespace),
		),
		resource.WithFromEnv(),
		resource.WithAttributes(explicit...),
	)
	if err != nil {
		return nil, fmt.Errorf("build OpenTelemetry resource: %w", err)
	}

	return res, nil
}

// explicitResourceAttributes collects attributes that override env-derived ones:
// flag-provided raw, programmatic map, and individual service fields. Order
// inside this slice matters — later entries override earlier ones for the same
// key when passed to resource.WithAttributes.
func explicitResourceAttributes(opts *Options) ([]attribute.KeyValue, error) {
	attrs := map[string]string{}

	rawAttrs, err := ParseResourceAttributesStrict(opts.ResourceAttributesRaw)
	if err != nil {
		return nil, fmt.Errorf("parse resource attributes option: %w", err)
	}

	maps.Copy(attrs, rawAttrs)
	maps.Copy(attrs, opts.ResourceAttributes)

	if opts.ServiceName != "" {
		attrs[serviceNameAttr] = opts.ServiceName
	}

	if opts.ServiceNamespace != "" {
		attrs[serviceNamespaceAttr] = opts.ServiceNamespace
	}

	if opts.ServiceVersion != "" {
		attrs["service.version"] = opts.ServiceVersion
	}

	if opts.DeploymentEnvironmentName != "" {
		attrs["deployment.environment.name"] = opts.DeploymentEnvironmentName
	}

	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for key, value := range attrs {
		kvs = append(kvs, attribute.String(key, value))
	}

	return kvs, nil
}

// ParseResourceAttributesStrict parses comma-separated OpenTelemetry attributes.
func ParseResourceAttributesStrict(raw string) (map[string]string, error) {
	attrs := map[string]string{}

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		key, value, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)

		if !ok || key == "" {
			return nil, fmt.Errorf("invalid resource attribute %q", part)
		}

		value = strings.TrimSpace(value)

		decodedValue, err := url.PathUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("invalid resource attribute %q: %w", part, err)
		}

		attrs[key] = decodedValue
	}

	return attrs, nil
}

// slogLevelTrace is the slog level reserved for trace messages.
const slogLevelTrace slog.Level = -8

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case LevelTrace:
		return slogLevelTrace, nil
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo, "":
		return slog.LevelInfo, nil
	case LevelWarn, "warning":
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level %q", level)
	}
}

func instrumentationName(opts *Options) string {
	if opts.InstrumentationName != "" {
		return opts.InstrumentationName
	}

	return DefaultServiceName
}

type levelHandler struct {
	handler slog.Handler
	min     slog.Level
}

func (h levelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.min
}

//nolint:gocritic // slog.Handler requires Handle to accept slog.Record by value.
func (h levelHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < h.min {
		return nil
	}

	normalizeSeverity(&record)

	if err := h.handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("handle slog record: %w", err)
	}

	return nil
}

func (h levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelHandler{handler: h.handler.WithAttrs(attrs), min: h.min}
}

func (h levelHandler) WithGroup(name string) slog.Handler {
	return levelHandler{handler: h.handler.WithGroup(name), min: h.min}
}

func normalizeSeverity(record *slog.Record) {
	switch {
	case record.Level <= slogLevelTrace:
		record.Level = slogLevelTrace
	case record.Level < slog.LevelInfo:
		record.Level = slog.LevelDebug
	}
}
