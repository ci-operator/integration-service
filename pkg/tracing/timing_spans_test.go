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

package tracing_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/konflux-ci/integration-service/pkg/tracing"
	"github.com/konflux-ci/integration-service/tekton/consts"
)

// testExporter is a simple in-memory exporter for testing
type testExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *testExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *testExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (e *testExporter) GetSpans() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spans
}

func (e *testExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = nil
}

var _ = Describe("Timing Spans", func() {
	var (
		exporter *testExporter
		provider *sdktrace.TracerProvider
	)

	BeforeEach(func() {
		// Set up in-memory exporter for testing
		exporter = &testExporter{}
		provider = sdktrace.NewTracerProvider(
			sdktrace.WithSyncer(exporter),
		)
		otel.SetTracerProvider(provider)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})

	AfterEach(func() {
		exporter.Reset()
		_ = provider.Shutdown(context.Background())
	})

	Describe("CtxFromSpanContext", func() {
		It("returns background context and false for empty string", func() {
			ctx, valid := tracing.CtxFromSpanContext("")
			Expect(valid).To(BeFalse())
			Expect(trace.SpanContextFromContext(ctx).IsValid()).To(BeFalse())
		})

		It("returns background context and false for invalid JSON", func() {
			ctx, valid := tracing.CtxFromSpanContext("not-json")
			Expect(valid).To(BeFalse())
			Expect(trace.SpanContextFromContext(ctx).IsValid()).To(BeFalse())
		})

		It("returns valid context and true for valid W3C traceparent", func() {
			// Valid W3C traceparent format: version-traceid-spanid-flags
			validSpanContext := "{\"traceparent\":\"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\"}"
			ctx, valid := tracing.CtxFromSpanContext(validSpanContext)
			Expect(valid).To(BeTrue())
			sc := trace.SpanContextFromContext(ctx)
			Expect(sc.IsValid()).To(BeTrue())
			Expect(sc.TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
			Expect(sc.SpanID().String()).To(Equal("00f067aa0ba902b7"))
		})
	})

	Describe("EmitWaitDuration", func() {
		It("does nothing if StartTime is nil", func() {
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Status: tektonv1.PipelineRunStatus{},
			}
			tracing.EmitWaitDuration(context.Background(), pr, "test")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("does nothing if end time is before start time", func() {
			now := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(now),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: now.Add(-time.Minute)}, // Before creation
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, "test")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("emits span with correct name and timestamps", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
					Labels: map[string]string{
						consts.PipelineRunTypeLabel: "test",
					},
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, "test")

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name()).To(Equal("waitDuration"))
			Expect(spans[0].StartTime()).To(BeTemporally("~", creationTime, time.Second))
			Expect(spans[0].EndTime()).To(BeTemporally("~", startTime, time.Second))
		})
	})

	Describe("EmitExecuteDuration", func() {
		It("does nothing if StartTime is nil", func() {
			pr := &tektonv1.PipelineRun{
				Status: tektonv1.PipelineRunStatus{},
			}
			tracing.EmitExecuteDuration(context.Background(), pr, "test")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("does nothing if CompletionTime is nil", func() {
			pr := &tektonv1.PipelineRun{
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: time.Now()},
					},
				},
			}
			tracing.EmitExecuteDuration(context.Background(), pr, "test")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("does nothing if end time is before start time", func() {
			now := time.Now()
			pr := &tektonv1.PipelineRun{
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: now},
						CompletionTime: &metav1.Time{Time: now.Add(-time.Minute)}, // Before start
					},
				},
			}
			tracing.EmitExecuteDuration(context.Background(), pr, "test")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("emits span with correct name and timestamps", func() {
			startTime := time.Now().Add(-time.Minute)
			completionTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.PipelineRunTypeLabel: "test",
					},
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: startTime},
						CompletionTime: &metav1.Time{Time: completionTime},
					},
				},
			}
			tracing.EmitExecuteDuration(context.Background(), pr, "test")

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name()).To(Equal("executeDuration"))
			Expect(spans[0].StartTime()).To(BeTemporally("~", startTime, time.Second))
			Expect(spans[0].EndTime()).To(BeTemporally("~", completionTime, time.Second))
		})
	})

	Describe("EmitTimingSpans", func() {
		It("returns false if StartTime is nil", func() {
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Status: tektonv1.PipelineRunStatus{},
			}
			result := tracing.EmitTimingSpans(pr, "test", "")
			Expect(result).To(BeFalse())
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("returns true and emits only waitDuration if CompletionTime is nil", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			result := tracing.EmitTimingSpans(pr, "test", "")
			Expect(result).To(BeTrue())

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name()).To(Equal("waitDuration"))
		})

		It("returns true and emits both spans if CompletionTime is set", func() {
			creationTime := time.Now().Add(-2 * time.Minute)
			startTime := time.Now().Add(-time.Minute)
			completionTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: startTime},
						CompletionTime: &metav1.Time{Time: completionTime},
					},
				},
			}
			result := tracing.EmitTimingSpans(pr, "test", "")
			Expect(result).To(BeTrue())

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(2))
			spanNames := []string{spans[0].Name(), spans[1].Name()}
			Expect(spanNames).To(ContainElements("waitDuration", "executeDuration"))
		})

		It("works with empty span context", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			result := tracing.EmitTimingSpans(pr, "test", "")
			Expect(result).To(BeTrue())
			Expect(exporter.GetSpans()).To(HaveLen(1))
		})

		It("uses parent context from valid span context", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			validSpanContext := "{\"traceparent\":\"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\"}"
			result := tracing.EmitTimingSpans(pr, "test", validSpanContext)
			Expect(result).To(BeTrue())

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			// The span should have the parent trace ID from the span context
			Expect(spans[0].Parent().TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		})
	})
})
