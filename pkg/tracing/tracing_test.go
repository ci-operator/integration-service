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
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"

	"github.com/konflux-ci/integration-service/pkg/tracing"
)

var _ = Describe("ResultEnum", func() {
	DescribeTable("maps PipelineRun Succeeded condition to the semconv enum",
		func(status corev1.ConditionStatus, reason, want string) {
			cond := &apis.Condition{Status: status, Reason: reason}
			Expect(tracing.ResultEnum(cond).Value.AsString()).To(Equal(want))
		},
		Entry("success",
			corev1.ConditionTrue, tektonv1.PipelineRunReasonSuccessful.String(),
			semconv.CICDPipelineResultSuccess.Value.AsString()),
		Entry("completed is also success",
			corev1.ConditionTrue, tektonv1.PipelineRunReasonCompleted.String(),
			semconv.CICDPipelineResultSuccess.Value.AsString()),
		Entry("failure",
			corev1.ConditionFalse, tektonv1.PipelineRunReasonFailed.String(),
			semconv.CICDPipelineResultFailure.Value.AsString()),
		Entry("timeout",
			corev1.ConditionFalse, tektonv1.PipelineRunReasonTimedOut.String(),
			semconv.CICDPipelineResultTimeout.Value.AsString()),
		Entry("cancelled",
			corev1.ConditionFalse, tektonv1.PipelineRunReasonCancelled.String(),
			semconv.CICDPipelineResultCancellation.Value.AsString()),
		Entry("cancelled-running-finally",
			corev1.ConditionFalse, tektonv1.PipelineRunReasonCancelledRunningFinally.String(),
			semconv.CICDPipelineResultCancellation.Value.AsString()),
		Entry("validation failure falls back to error",
			corev1.ConditionFalse, tektonv1.PipelineRunReasonFailedValidation.String(),
			semconv.CICDPipelineResultError.Value.AsString()),
		Entry("unknown reason falls back to error",
			corev1.ConditionFalse, "SomeFutureTektonReason",
			semconv.CICDPipelineResultError.Value.AsString()),
	)
})

var _ = Describe("TruncateResultMessage", func() {
	It("passes through a short message unchanged", func() {
		Expect(tracing.TruncateResultMessage("short")).To(Equal("short"))
	})

	It("passes through a message exactly at the limit unchanged", func() {
		msg := strings.Repeat("a", tracing.MaxResultMessageLen)
		Expect(tracing.TruncateResultMessage(msg)).To(Equal(msg))
	})

	It("truncates and suffixes a message over the limit", func() {
		msg := strings.Repeat("a", tracing.MaxResultMessageLen+50)
		got := tracing.TruncateResultMessage(msg)
		Expect(len(got)).To(BeNumerically("<=", tracing.MaxResultMessageLen))
		Expect(got).To(HaveSuffix(tracing.TruncatedSuffix))
	})

	It("preserves UTF-8 boundaries when truncating", func() {
		prefix := strings.Repeat("a", tracing.MaxResultMessageLen-len(tracing.TruncatedSuffix)-2)
		msg := prefix + "世" + strings.Repeat("b", 100)
		got := tracing.TruncateResultMessage(msg)
		head := strings.TrimSuffix(got, tracing.TruncatedSuffix)
		Expect(head).NotTo(Equal(got))
		for _, r := range head {
			Expect(r).NotTo(Equal('�'))
		}
	})
})

var _ = Describe("EarliestFailingTaskRunMessage", func() {
	var (
		t1 metav1.Time
		t2 metav1.Time
		t3 metav1.Time
	)

	BeforeEach(func() {
		now := time.Now()
		t1 = metav1.NewTime(now.Add(-3 * time.Minute))
		t2 = metav1.NewTime(now.Add(-2 * time.Minute))
		t3 = metav1.NewTime(now.Add(-time.Minute))
	})

	mkTR := func(name string, completion *metav1.Time, status corev1.ConditionStatus, message string) *tektonv1.TaskRun {
		tr := &tektonv1.TaskRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		}
		tr.Status.CompletionTime = completion
		if completion != nil {
			tr.Status.Conditions = duckv1.Conditions{{
				Type:    apis.ConditionSucceeded,
				Status:  status,
				Message: message,
			}}
		}
		return tr
	}

	mkPR := func(names ...string) *tektonv1.PipelineRun {
		pr := &tektonv1.PipelineRun{
			ObjectMeta: metav1.ObjectMeta{Name: "pr", Namespace: "ns"},
		}
		for _, n := range names {
			pr.Status.ChildReferences = append(pr.Status.ChildReferences, tektonv1.ChildStatusReference{Name: n})
		}
		return pr
	}

	It("returns empty when the PipelineRun has no child references", func() {
		got := tracing.EarliestFailingTaskRunMessage(context.Background(), newFakeClient(), mkPR())
		Expect(got).To(Equal(""))
	})

	It("returns empty when the only child succeeded", func() {
		c := newFakeClient(mkTR("tr-ok", &t2, corev1.ConditionTrue, "done"))
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-ok"))).To(Equal(""))
	})

	It("returns a single failed child's message", func() {
		c := newFakeClient(mkTR("tr-fail", &t2, corev1.ConditionFalse, "boom"))
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-fail"))).To(Equal("boom"))
	})

	It("returns the earliest failing message when multiple children fail", func() {
		c := newFakeClient(
			mkTR("tr-late", &t3, corev1.ConditionFalse, "late"),
			mkTR("tr-early", &t1, corev1.ConditionFalse, "early"),
		)
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-late", "tr-early"))).To(Equal("early"))
	})

	It("picks the earliest failing child when success and failure are mixed", func() {
		c := newFakeClient(
			mkTR("tr-ok", &t1, corev1.ConditionTrue, "done"),
			mkTR("tr-fail", &t2, corev1.ConditionFalse, "bad"),
		)
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-ok", "tr-fail"))).To(Equal("bad"))
	})

	It("skips incomplete children", func() {
		c := newFakeClient(
			mkTR("tr-running", nil, corev1.ConditionUnknown, ""),
			mkTR("tr-fail", &t2, corev1.ConditionFalse, "later"),
		)
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-running", "tr-fail"))).To(Equal("later"))
	})

	It("silently skips a TaskRun missing from the cluster", func() {
		c := newFakeClient(mkTR("tr-fail", &t2, corev1.ConditionFalse, "present-failure"))
		Expect(tracing.EarliestFailingTaskRunMessage(context.Background(), c, mkPR("tr-fail", "tr-missing"))).To(Equal("present-failure"))
	})
})

var _ = Describe("LoadLabelNames", func() {
	envKeys := []string{
		tracing.EnvTracingLabelAction,
		tracing.EnvTracingLabelApplication,
		tracing.EnvTracingLabelComponent,
	}

	snapshotEnv := func() map[string]string {
		saved := map[string]string{}
		for _, k := range envKeys {
			if v, ok := os.LookupEnv(k); ok {
				saved[k] = v
			}
		}
		return saved
	}

	restoreEnv := func(saved map[string]string) {
		for _, k := range envKeys {
			_ = os.Unsetenv(k)
		}
		for k, v := range saved {
			_ = os.Setenv(k, v)
		}
	}

	It("returns the delivery.tekton.dev/* defaults when env vars are unset", func() {
		saved := snapshotEnv()
		DeferCleanup(restoreEnv, saved)
		for _, k := range envKeys {
			_ = os.Unsetenv(k)
		}
		got := tracing.LoadLabelNames()
		Expect(got.Action).To(Equal(tracing.DefaultTracingLabelAction))
		Expect(got.Application).To(Equal(tracing.DefaultTracingLabelApplication))
		Expect(got.Component).To(Equal(tracing.DefaultTracingLabelComponent))
	})

	It("treats an explicit empty-string as disabling the attribute", func() {
		saved := snapshotEnv()
		DeferCleanup(restoreEnv, saved)
		for _, k := range envKeys {
			_ = os.Setenv(k, "")
		}
		got := tracing.LoadLabelNames()
		Expect(got.Action).To(Equal(""))
		Expect(got.Application).To(Equal(""))
		Expect(got.Component).To(Equal(""))
	})
})
