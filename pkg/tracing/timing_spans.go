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
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"knative.dev/pkg/apis"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	"github.com/konflux-ci/integration-service/tekton/consts"
)

// CtxFromSpanContext creates a context with the trace as parent.
// Returns the context and true if a valid span context was extracted, otherwise
// returns a background context and false.
func CtxFromSpanContext(jsonCarrier string) (context.Context, bool) {
	if jsonCarrier == "" {
		return context.Background(), false
	}
	var carrier map[string]string
	if err := json.Unmarshal([]byte(jsonCarrier), &carrier); err != nil {
		return context.Background(), false
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(carrier))
	sc := trace.SpanContextFromContext(ctx)
	return ctx, sc.IsValid()
}

// setCommonAttributes adds identity and stage attributes to all spans
func setCommonAttributes(span trace.Span, pr *tektonv1.PipelineRun, phase string) {
	span.SetAttributes(
		semconv.K8SNamespaceName(pr.Namespace),
		TektonPipelineRunNameKey.String(pr.Name),
		TektonPipelineRunUIDKey.String(string(pr.UID)),
		DeliveryStageKey.String(phase),
	)
	if app, ok := pr.Labels[consts.PipelineRunApplicationLabel]; ok && app != "" {
		span.SetAttributes(DeliveryApplicationKey.String(app))
	}
	if comp, ok := pr.Labels[consts.PipelineRunComponentLabel]; ok && comp != "" {
		span.SetAttributes(DeliveryComponentKey.String(comp))
	}
	if pipelineType, ok := pr.Labels[consts.PipelineRunTypeLabel]; ok && pipelineType != "" {
		span.SetAttributes(DeliveryPipelineRole.String(pipelineType))
	}
}

// setOutcomeAttributes adds success/reason attributes (only for executeDuration)
func setOutcomeAttributes(span trace.Span, pr *tektonv1.PipelineRun) {
	condition := pr.Status.GetCondition(apis.ConditionSucceeded)
	success := false
	reason := ""
	if condition != nil {
		success = condition.IsTrue()
		reason = condition.Reason
	}
	span.SetAttributes(
		DeliverySuccessKey.Bool(success),
		DeliveryReasonKey.String(reason),
	)
}

// EmitWaitDuration emits a waitDuration span for a PipelineRun.
// The span represents the time from creation until execution started:
// Start = creationTimestamp, End = status.startTime
func EmitWaitDuration(ctx context.Context, pr *tektonv1.PipelineRun, phase string) {
	if pr.Status.StartTime == nil {
		return
	}
	start := pr.CreationTimestamp.Time
	end := pr.Status.StartTime.Time
	if end.Before(start) {
		return
	}

	tr := otel.Tracer(TracerName)
	_, span := tr.Start(ctx, "waitDuration",
		trace.WithTimestamp(start),
	)

	setCommonAttributes(span, pr, phase)
	span.End(trace.WithTimestamp(end))
}

// EmitExecuteDuration emits an executeDuration span for a PipelineRun.
// The span represents the actual execution time:
// Start = status.startTime, End = status.completionTime
func EmitExecuteDuration(ctx context.Context, pr *tektonv1.PipelineRun, phase string) {
	if pr.Status.StartTime == nil || pr.Status.CompletionTime == nil {
		return
	}
	start := pr.Status.StartTime.Time
	end := pr.Status.CompletionTime.Time
	if end.Before(start) {
		return
	}

	tr := otel.Tracer(TracerName)
	_, span := tr.Start(ctx, "executeDuration",
		trace.WithTimestamp(start),
	)

	setCommonAttributes(span, pr, phase)
	setOutcomeAttributes(span, pr)
	span.End(trace.WithTimestamp(end))
}

// EmitTimingSpans emits both waitDuration and executeDuration spans for a PipelineRun.
// The spans are parented under the delivery trace context if a valid span context is provided.
// Returns true if spans were emitted (even if to a noop provider), false if the PipelineRun
// doesn't have the required timestamps.
func EmitTimingSpans(pr *tektonv1.PipelineRun, phase string, spanContext string) bool {
	// Skip if tracing is not configured
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		return false
	}

	// Get parent context from span context
	parentCtx, ok := CtxFromSpanContext(spanContext)
	if !ok {
		parentCtx = context.Background()
	}

	// Check if we have enough data to emit spans
	if pr.Status.StartTime == nil {
		return false
	}

	EmitWaitDuration(parentCtx, pr, phase)

	// Only emit execute_duration if the PipelineRun has completed
	if pr.Status.CompletionTime != nil {
		EmitExecuteDuration(parentCtx, pr, phase)
	}

	return true
}
