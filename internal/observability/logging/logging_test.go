package logging

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
)

func TestBuildResourceHonorsOTelEnvAndExplicitPrecedence(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=from-resource-env,env.only=env,shared=env,service.version=env-version")
	t.Setenv("OTEL_SERVICE_NAME", "from-service-env")

	res, err := buildResource(context.Background(), &Options{
		ServiceName:           "explicit-service",
		ServiceVersion:        "explicit-version",
		ResourceAttributesRaw: "raw.only=raw,shared=raw",
		ResourceAttributes: map[string]string{
			"map.only":     "map",
			"service.name": "map-service",
			"shared":       "map",
		},
	})
	if err != nil {
		t.Fatalf("buildResource() error = %v", err)
	}

	attrs := resourceAttrs(res)
	assertAttr(t, attrs, serviceNameAttr, "explicit-service")
	assertAttr(t, attrs, "env.only", "env")
	assertAttr(t, attrs, "raw.only", "raw")
	assertAttr(t, attrs, "map.only", "map")
	assertAttr(t, attrs, "shared", "map")
	assertAttr(t, attrs, "service.version", "explicit-version")
}

func TestOTelServiceNameOverridesOTelResourceServiceName(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=from-resource-env")
	t.Setenv("OTEL_SERVICE_NAME", "from-service-env")

	res, err := buildResource(context.Background(), &Options{})
	if err != nil {
		t.Fatalf("buildResource() error = %v", err)
	}

	assertAttr(t, resourceAttrs(res), serviceNameAttr, "from-service-env")
}

func TestDefaultServiceNameUsedWithoutEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	res, err := buildResource(context.Background(), &Options{})
	if err != nil {
		t.Fatalf("buildResource() error = %v", err)
	}

	assertAttr(t, resourceAttrs(res), serviceNameAttr, DefaultServiceName)
}

func TestLevelFilteringForLogrVerbosity(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		v0Enabled bool
		v4Enabled bool
		v8Enabled bool
	}{
		{name: "info", level: LevelInfo, v0Enabled: true, v4Enabled: false, v8Enabled: false},
		{name: "debug", level: LevelDebug, v0Enabled: true, v4Enabled: true, v8Enabled: false},
		{name: "trace", level: LevelTrace, v0Enabled: true, v4Enabled: true, v8Enabled: true},
		{name: "warn", level: LevelWarn, v0Enabled: false, v4Enabled: false, v8Enabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, logger, shutdown, err := NewLogger(context.Background(), &Options{
				Level:    tt.level,
				Exporter: ExporterNone,
			})
			if err != nil {
				t.Fatalf("NewLogger() error = %v", err)
			}

			defer func() {
				if err := shutdown(context.Background()); err != nil {
					t.Fatalf("shutdown() error = %v", err)
				}
			}()

			if provider == nil {
				t.Fatal("provider is nil")
			}

			if got := logger.Enabled(); got != tt.v0Enabled {
				t.Fatalf("logger.Enabled() = %v, want %v", got, tt.v0Enabled)
			}

			if got := logger.V(4).Enabled(); got != tt.v4Enabled {
				t.Fatalf("logger.V(4).Enabled() = %v, want %v", got, tt.v4Enabled)
			}

			if got := logger.V(8).Enabled(); got != tt.v8Enabled {
				t.Fatalf("logger.V(8).Enabled() = %v, want %v", got, tt.v8Enabled)
			}
		})
	}
}

func TestConsoleExporterWritesLogRecord(t *testing.T) {
	var buf bytes.Buffer

	provider, logger, shutdown, err := NewLogger(context.Background(), &Options{
		Level:         LevelInfo,
		Exporter:      ExporterConsole,
		ConsoleWriter: &buf,
		ServiceName:   "test-service",
	})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	logger.Info("hello from logging test", "key", "value")

	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "hello from logging test") {
		t.Fatalf("console output %q does not contain log message", output)
	}

	if !strings.Contains(output, "test-service") {
		t.Fatalf("console output %q does not contain service name", output)
	}
}

func TestInvalidOptionsReturnErrors(t *testing.T) {
	tests := []Options{
		{Level: "verbose", Exporter: ExporterNone},
		{Level: LevelInfo, Exporter: "file"},
		{Level: LevelInfo, Exporter: ExporterNone, ResourceAttributesRaw: "missing-value"},
	}

	for _, opts := range tests {
		if _, _, _, err := NewLogger(context.Background(), &opts); err == nil {
			t.Fatalf("NewLogger(%+v) error = nil, want error", opts)
		}
	}
}

func resourceAttrs(res *resource.Resource) map[string]string {
	attrs := make(map[string]string)
	for _, attr := range res.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}

	return attrs
}

func assertAttr(t *testing.T, attrs map[string]string, key, want string) {
	t.Helper()

	if got, ok := attrs[key]; !ok || got != want {
		t.Fatalf("attribute %q = %q (present %v), want %q", key, got, ok, want)
	}
}
