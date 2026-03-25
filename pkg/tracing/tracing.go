/*
Copyright 2024 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// TracerName is the name of the tracer for integration-service
	TracerName = "integration-service"

	// EnvOTLPEndpoint is the environment variable for the OTLP endpoint
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

// Span attribute keys.
var (
	DeliveryStageKey       = attribute.Key("delivery.stage")
	DeliveryApplicationKey = attribute.Key("delivery.application")
	DeliveryComponentKey   = attribute.Key("delivery.component")
	DeliverySuccessKey     = attribute.Key("delivery.success")
	DeliveryReasonKey      = attribute.Key("delivery.reason")
	DeliveryPipelineRole   = attribute.Key("delivery.pipeline.role")

	TektonPipelineRunNameKey = attribute.Key("tekton.pipelinerun.name")
	TektonPipelineRunUIDKey  = attribute.Key("tekton.pipelinerun.uid")
)

var setupLog = ctrl.Log.WithName("tracing")

// TracerProvider wraps the OpenTelemetry tracer provider
type TracerProvider struct {
	provider trace.TracerProvider
	shutdown func(context.Context) error
}

// New creates a new TracerProvider. If the OTLP endpoint is not configured,
// it returns a noop tracer provider.
func New(ctx context.Context) (*TracerProvider, error) {
	endpoint := os.Getenv(EnvOTLPEndpoint)
	if endpoint == "" {
		setupLog.Info("OTLP endpoint not configured, using noop tracer provider")
		return &TracerProvider{
			provider: trace.NewNoopTracerProvider(),
			shutdown: func(context.Context) error { return nil },
		}, nil
	}

	// Create OTLP exporter
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // TODO: make TLS configurable
	)
	if err != nil {
		setupLog.Error(err, "failed to create OTLP exporter, using noop tracer provider")
		return &TracerProvider{
			provider: trace.NewNoopTracerProvider(),
			shutdown: func(context.Context) error { return nil },
		}, nil
	}

	// Create resource with service name
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(TracerName),
		),
	)
	if err != nil {
		setupLog.Error(err, "failed to create resource")
		res = resource.Default()
	}

	// Create tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Set global tracer provider and propagator
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	setupLog.Info("tracing initialized", "endpoint", endpoint)

	return &TracerProvider{
		provider: tp,
		shutdown: tp.Shutdown,
	}, nil
}

// Tracer returns a tracer from the provider
func (tp *TracerProvider) Tracer() trace.Tracer {
	return tp.provider.Tracer(TracerName)
}

// Shutdown shuts down the tracer provider
func (tp *TracerProvider) Shutdown(ctx context.Context) error {
	if tp.shutdown != nil {
		return tp.shutdown(ctx)
	}
	return nil
}

// IsEnabled returns true if tracing is enabled (not using noop provider)
func (tp *TracerProvider) IsEnabled() bool {
	_, ok := tp.provider.(*sdktrace.TracerProvider)
	return ok
}
