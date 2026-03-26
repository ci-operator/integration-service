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
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/konflux-ci/integration-service/pkg/tracing"
)

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

func (e *testExporter) Shutdown(ctx context.Context) error { return nil }

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

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, attr := range s.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.Emit()
		}
	}
	return ""
}

func hasAttr(s sdktrace.ReadOnlySpan, key string) bool {
	for _, attr := range s.Attributes() {
		if string(attr.Key) == key {
			return true
		}
	}
	return false
}

func defaultLabels() tracing.LabelNames {
	return tracing.LabelNames{
		Action:      tracing.DefaultTracingLabelAction,
		Application: tracing.DefaultTracingLabelApplication,
		Component:   tracing.DefaultTracingLabelComponent,
	}
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = tektonv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

var _ = Describe("Timing Spans", func() {
	var (
		exporter *testExporter
		provider *sdktrace.TracerProvider
	)

	BeforeEach(func() {
		exporter = &testExporter{}
		provider = sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
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
			validSpanContext := `{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`
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
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Now()},
			}
			tracing.EmitWaitDuration(context.Background(), pr, defaultLabels())
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("does nothing if end time is before start time", func() {
			now := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: now.Add(-time.Minute)},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, defaultLabels())
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("emits span with correct name and timestamps", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(creationTime)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, defaultLabels())
			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name()).To(Equal(tracing.SpanWaitDuration))
			Expect(spans[0].StartTime()).To(BeTemporally("~", creationTime, time.Second))
			Expect(spans[0].EndTime()).To(BeTemporally("~", startTime, time.Second))
		})
	})

	Describe("EmitExecuteDuration", func() {
		It("does nothing if StartTime is nil", func() {
			pr := &tektonv1.PipelineRun{}
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
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
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("emits span with correct name and timestamps", func() {
			startTime := time.Now().Add(-time.Minute)
			completionTime := time.Now()
			pr := &tektonv1.PipelineRun{
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: startTime},
						CompletionTime: &metav1.Time{Time: completionTime},
					},
				},
			}
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name()).To(Equal(tracing.SpanExecuteDuration))
			Expect(spans[0].StartTime()).To(BeTemporally("~", startTime, time.Second))
			Expect(spans[0].EndTime()).To(BeTemporally("~", completionTime, time.Second))
		})
	})

	Describe("EmitTimingSpans", func() {
		It("returns false if StartTime is nil", func() {
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Now()},
			}
			result := tracing.EmitTimingSpans(context.Background(), newFakeClient(), pr, defaultLabels(), "")
			Expect(result).To(BeFalse())
			Expect(exporter.GetSpans()).To(BeEmpty())
		})

		It("returns true and emits only waitDuration if CompletionTime is nil", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(creationTime)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			result := tracing.EmitTimingSpans(context.Background(), newFakeClient(), pr, defaultLabels(), "")
			Expect(result).To(BeTrue())
			Expect(exporter.GetSpans()).To(HaveLen(1))
			Expect(exporter.GetSpans()[0].Name()).To(Equal(tracing.SpanWaitDuration))
		})

		It("returns true and emits both spans if CompletionTime is set", func() {
			creationTime := time.Now().Add(-2 * time.Minute)
			startTime := time.Now().Add(-time.Minute)
			completionTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(creationTime)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: startTime},
						CompletionTime: &metav1.Time{Time: completionTime},
					},
				},
			}
			result := tracing.EmitTimingSpans(context.Background(), newFakeClient(), pr, defaultLabels(), "")
			Expect(result).To(BeTrue())
			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(2))
			Expect([]string{spans[0].Name(), spans[1].Name()}).To(ContainElements(tracing.SpanWaitDuration, tracing.SpanExecuteDuration))
		})

		It("uses parent context from valid span context", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(creationTime)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			validSpanContext := `{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`
			result := tracing.EmitTimingSpans(context.Background(), newFakeClient(), pr, defaultLabels(), validSpanContext)
			Expect(result).To(BeTrue())
			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Parent().TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		})
	})

	Describe("Label-driven attribute emission", func() {
		It("emits identity + label attributes on waitDuration using the configured label names", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-pr",
					Namespace:         "test-ns",
					UID:               types.UID("test-uid-123"),
					CreationTimestamp: metav1.NewTime(creationTime),
					Labels: map[string]string{
						tracing.DefaultTracingLabelAction:      "build",
						tracing.DefaultTracingLabelApplication: "my-app",
						tracing.DefaultTracingLabelComponent:   "my-comp",
					},
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, defaultLabels())
			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			s := spans[0]
			Expect(spanAttr(s, string(tracing.NamespaceKey))).To(Equal("test-ns"))
			Expect(spanAttr(s, string(tracing.PipelineRunKey))).To(Equal("test-pr"))
			Expect(spanAttr(s, string(tracing.DeliveryPipelineRunUIDKey))).To(Equal("test-uid-123"))
			Expect(spanAttr(s, string(semconv.CICDPipelineActionNameKey))).To(Equal("build"))
			Expect(spanAttr(s, string(tracing.DeliveryApplicationKey))).To(Equal("my-app"))
			Expect(spanAttr(s, string(tracing.DeliveryComponentKey))).To(Equal("my-comp"))
		})

		It("omits attributes when labels are missing from the PipelineRun", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(creationTime)},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, defaultLabels())
			s := exporter.GetSpans()[0]
			Expect(hasAttr(s, string(semconv.CICDPipelineActionNameKey))).To(BeFalse())
			Expect(hasAttr(s, string(tracing.DeliveryApplicationKey))).To(BeFalse())
			Expect(hasAttr(s, string(tracing.DeliveryComponentKey))).To(BeFalse())
		})

		It("skips a configured-empty label name without reading any label", func() {
			creationTime := time.Now().Add(-time.Minute)
			startTime := time.Now()
			partial := tracing.LabelNames{
				Action:      "",
				Application: tracing.DefaultTracingLabelApplication,
				Component:   tracing.DefaultTracingLabelComponent,
			}
			pr := &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(creationTime),
					Labels: map[string]string{
						tracing.DefaultTracingLabelAction:      "should-not-be-emitted",
						tracing.DefaultTracingLabelApplication: "my-app",
					},
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime: &metav1.Time{Time: startTime},
					},
				},
			}
			tracing.EmitWaitDuration(context.Background(), pr, partial)
			s := exporter.GetSpans()[0]
			Expect(hasAttr(s, string(semconv.CICDPipelineActionNameKey))).To(BeFalse())
			Expect(spanAttr(s, string(tracing.DeliveryApplicationKey))).To(Equal("my-app"))
		})
	})

	Describe("Outcome attributes", func() {
		makeCompletedPR := func(status corev1.ConditionStatus, reason string, message string) *tektonv1.PipelineRun {
			startTime := time.Now().Add(-time.Minute)
			completionTime := time.Now()
			return &tektonv1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-pr",
					Namespace:         "test-ns",
					CreationTimestamp: metav1.NewTime(startTime.Add(-30 * time.Second)),
				},
				Status: tektonv1.PipelineRunStatus{
					PipelineRunStatusFields: tektonv1.PipelineRunStatusFields{
						StartTime:      &metav1.Time{Time: startTime},
						CompletionTime: &metav1.Time{Time: completionTime},
					},
					Status: duckv1.Status{
						Conditions: duckv1.Conditions{{
							Type:    apis.ConditionSucceeded,
							Status:  status,
							Reason:  reason,
							Message: message,
						}},
					},
				},
			}
		}

		It("maps successful PipelineRun to success and omits result_message", func() {
			pr := makeCompletedPR(corev1.ConditionTrue, tektonv1.PipelineRunReasonSuccessful.String(), "All steps completed")
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			s := exporter.GetSpans()[0]
			Expect(spanAttr(s, string(semconv.CICDPipelineResultKey))).To(Equal(semconv.CICDPipelineResultSuccess.Value.AsString()))
			Expect(hasAttr(s, string(tracing.DeliveryResultMessageKey))).To(BeFalse())
		})

		It("maps failed PipelineRun to failure and uses the supplied failure message", func() {
			pr := makeCompletedPR(corev1.ConditionFalse, tektonv1.PipelineRunReasonFailed.String(), "pr-level fallback")
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "failing-taskrun exit 1")
			s := exporter.GetSpans()[0]
			Expect(spanAttr(s, string(semconv.CICDPipelineResultKey))).To(Equal(semconv.CICDPipelineResultFailure.Value.AsString()))
			Expect(spanAttr(s, string(tracing.DeliveryResultMessageKey))).To(Equal("failing-taskrun exit 1"))
		})

		It("falls back to the PipelineRun condition message when no walker result is supplied", func() {
			pr := makeCompletedPR(corev1.ConditionFalse, tektonv1.PipelineRunReasonFailedValidation.String(), "validation failed")
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			s := exporter.GetSpans()[0]
			Expect(spanAttr(s, string(semconv.CICDPipelineResultKey))).To(Equal(semconv.CICDPipelineResultError.Value.AsString()))
			Expect(spanAttr(s, string(tracing.DeliveryResultMessageKey))).To(Equal("validation failed"))
		})

		It("maps timed-out PipelineRun to timeout", func() {
			pr := makeCompletedPR(corev1.ConditionFalse, tektonv1.PipelineRunReasonTimedOut.String(), "timed out")
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			Expect(spanAttr(exporter.GetSpans()[0], string(semconv.CICDPipelineResultKey))).To(Equal(semconv.CICDPipelineResultTimeout.Value.AsString()))
		})

		It("maps cancelled PipelineRun to cancellation", func() {
			pr := makeCompletedPR(corev1.ConditionFalse, tektonv1.PipelineRunReasonCancelled.String(), "")
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), "")
			Expect(spanAttr(exporter.GetSpans()[0], string(semconv.CICDPipelineResultKey))).To(Equal(semconv.CICDPipelineResultCancellation.Value.AsString()))
		})

		It("truncates an overlong failure message", func() {
			pr := makeCompletedPR(corev1.ConditionFalse, tektonv1.PipelineRunReasonFailed.String(), "")
			long := strings.Repeat("x", tracing.MaxResultMessageLen*2)
			tracing.EmitExecuteDuration(context.Background(), pr, defaultLabels(), long)
			emitted := spanAttr(exporter.GetSpans()[0], string(tracing.DeliveryResultMessageKey))
			Expect(len(emitted)).To(BeNumerically("<=", tracing.MaxResultMessageLen))
			Expect(emitted).To(HaveSuffix(tracing.TruncatedSuffix))
		})
	})
})
