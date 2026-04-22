/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/openkruise-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	kruiserolloutv1beta1 "github.com/openkruise/kruise-rollout-api/rollouts/v1beta1"
)

// setupRetryScenario builds the full linkage needed for retry detection:
// Kruise Rollout → Kustomization → kuberik Rollout. The returned helpers let
// each test tweak the individual pieces (stalled timestamps, retry cutoff,
// RolloutTest failure state) without rebuilding the scaffold.
type retryFixture struct {
	ns             string
	kruiseRollout  *kruiserolloutv1beta1.Rollout
	kustomization  *kustomizev1.Kustomization
	kuberikRollout *kuberikrolloutv1alpha1.Rollout
	rolloutTest    *rolloutv1alpha1.RolloutTest
	reconciler     *RolloutStepGateReconciler
}

func newRetryFixture(ctx context.Context) *retryFixture {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "retry-ns-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())

	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-ks",
			Namespace: ns.Name,
			Annotations: map[string]string{
				"rollout.kuberik.com/substitute.IMAGE_TAG.from": "app-rollout",
			},
		},
		Spec: kustomizev1.KustomizationSpec{
			Path: "./",
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Kind: "GitRepository",
				Name: "src",
			},
		},
	}
	Expect(k8sClient.Create(ctx, kustomization)).To(Succeed())

	kuberikRollout := &kuberikrolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: "app-rollout", Namespace: ns.Name},
		Spec: kuberikrolloutv1alpha1.RolloutSpec{
			ReleasesImagePolicy: corev1.LocalObjectReference{Name: "ignored"},
		},
	}
	Expect(k8sClient.Create(ctx, kuberikRollout)).To(Succeed())

	kruiseRollout := &kruiserolloutv1beta1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-kruise",
			Namespace: ns.Name,
			Annotations: map[string]string{
				"kustomize.toolkit.fluxcd.io/name":      "app-ks",
				"kustomize.toolkit.fluxcd.io/namespace": ns.Name,
			},
		},
		Spec: kruiserolloutv1beta1.RolloutSpec{
			Strategy: kruiserolloutv1beta1.RolloutStrategy{
				Canary: &kruiserolloutv1beta1.CanaryStrategy{
					Steps: []kruiserolloutv1beta1.CanaryStep{
						{
							Replicas: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
							Pause:    kruiserolloutv1beta1.RolloutPause{Duration: func() *int32 { d := int32(3600); return &d }()},
						},
					},
				},
			},
			WorkloadRef: kruiserolloutv1beta1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "app"},
		},
	}
	Expect(k8sClient.Create(ctx, kruiseRollout)).To(Succeed())
	kruiseRollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{CanaryRevision: "rev-1"}
	kruiseRollout.Status.CanaryStatus.CurrentStepIndex = 1
	kruiseRollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
	Expect(k8sClient.Status().Update(ctx, kruiseRollout)).To(Succeed())

	rolloutTest := &rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{Name: "app-test", Namespace: ns.Name},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: kruiseRollout.Name,
			StepIndex:   1,
			JobTemplate: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers:    []corev1.Container{{Name: "c", Image: "busybox", Command: []string{"true"}}},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, rolloutTest)).To(Succeed())

	return &retryFixture{
		ns:             ns.Name,
		kruiseRollout:  kruiseRollout,
		kustomization:  kustomization,
		kuberikRollout: kuberikRollout,
		rolloutTest:    rolloutTest,
		reconciler:     &RolloutStepGateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()},
	}
}

func (f *retryFixture) cleanup(ctx context.Context) {
	GinkgoHelper()
	Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.ns}})).To(Succeed())
}

// setKuberikRetry stamps LastRetryTimestamp (and mode) on the kuberik Rollout's
// latest history entry. retryAt is the simulated retry moment. mode is "retry" or
// "skip" (empty treated as retry by downstream logic).
func (f *retryFixture) setKuberikRetry(ctx context.Context, retryAt time.Time, mode string) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kuberikRollout.Name, Namespace: f.ns}, f.kuberikRollout)).To(Succeed())
	ts := metav1.NewTime(retryAt)
	deploying := kuberikrolloutv1alpha1.BakeStatusDeploying
	f.kuberikRollout.Status.History = []kuberikrolloutv1alpha1.DeploymentHistoryEntry{{
		Version:            kuberikrolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
		Timestamp:          metav1.NewTime(retryAt.Add(-30 * time.Minute)),
		BakeStatus:         &deploying,
		LastRetryTimestamp: &ts,
		LastRetryMode:      mode,
	}}
	Expect(k8sClient.Status().Update(ctx, f.kuberikRollout)).To(Succeed())
}

// setStalled puts a Stalled=True condition on the Kruise Rollout with the
// given reason and transition time.
func (f *retryFixture) setStalled(ctx context.Context, reason string, transitionAt time.Time) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, f.kruiseRollout)).To(Succeed())
	f.kruiseRollout.Status.Conditions = []kruiserolloutv1beta1.RolloutCondition{{
		Type:               kruiserolloutv1beta1.RolloutConditionType("Stalled"),
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		Message:            "stalled for test",
		LastTransitionTime: metav1.NewTime(transitionAt),
		LastUpdateTime:     metav1.NewTime(transitionAt),
	}}
	Expect(k8sClient.Status().Update(ctx, f.kruiseRollout)).To(Succeed())
}

// setTestFailed marks the RolloutTest as Failed with a Failed condition whose
// LastTransitionTime matches failedAt. Optionally creates an owned Job so we
// can verify the retry reset deletes it.
func (f *retryFixture) setTestFailed(ctx context.Context, failedAt time.Time, withJob bool) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, f.rolloutTest)).To(Succeed())
	f.rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseFailed
	f.rolloutTest.Status.ObservedCanaryRevision = "rev-1"
	f.rolloutTest.Status.JobName = "app-test-job"
	f.rolloutTest.Status.Conditions = []metav1.Condition{
		{
			Type:               "Failed",
			Status:             metav1.ConditionTrue,
			Reason:             "JobFailed",
			Message:            "test job failed",
			LastTransitionTime: metav1.NewTime(failedAt),
		},
		{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "JobFailed",
			Message:            "test job failed",
			LastTransitionTime: metav1.NewTime(failedAt),
		},
	}
	Expect(k8sClient.Status().Update(ctx, f.rolloutTest)).To(Succeed())

	if withJob {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-test-job",
				Namespace: f.ns,
				Labels:    map[string]string{"rollout-test": f.rolloutTest.Name},
			},
			Spec: f.rolloutTest.Spec.JobTemplate,
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
	}
}

var _ = Describe("RolloutStepGate retry handling", func() {
	ctx := context.Background()
	var f *retryFixture

	BeforeEach(func() {
		f = newRetryFixture(ctx)
	})
	AfterEach(func() {
		f.cleanup(ctx)
	})

	It("resets failed RolloutTests and clears Stalled when retry postdates stall", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-5 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt.Add(-1*time.Minute), true)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		var stalled *kruiserolloutv1beta1.RolloutCondition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				stalled = &updated.Status.Conditions[i]
			}
		}
		Expect(stalled).NotTo(BeNil())
		Expect(stalled.Status).To(Equal(corev1.ConditionFalse))

		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))
		Expect(updatedTest.Status.JobName).To(BeEmpty())

		// Owned Job should be deleted by the reset.
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})

	It("does not reset when stall postdates retry (fresh failure)", func() {
		retryAt := time.Now().Add(-30 * time.Minute)
		stallAt := time.Now().Add(-1 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt, false)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		var stalled *kruiserolloutv1beta1.RolloutCondition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				stalled = &updated.Status.Conditions[i]
			}
		}
		Expect(stalled).NotTo(BeNil())
		// Fresh failure should remain stalled — retry doesn't apply.
		Expect(stalled.Status).To(Equal(corev1.ConditionTrue))

		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseFailed))
	})

	It("is a no-op when kuberik Rollout has no LastRetryTimestamp", func() {
		stallAt := time.Now().Add(-10 * time.Minute)
		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt, false)
		// No setKuberikRetry — empty history

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseFailed))
	})

	It("clears KuberikRolloutBakeFailed stall when retry postdates it (no failed tests)", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-2 * time.Minute)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		var stalled *kruiserolloutv1beta1.RolloutCondition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				stalled = &updated.Status.Conditions[i]
			}
		}
		Expect(stalled).NotTo(BeNil())
		Expect(stalled.Status).To(Equal(corev1.ConditionFalse))
	})

	It("end-to-end: stall → retry → single reconcile clears stall and parks test for re-run", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-2 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt.Add(-1*time.Minute), true)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		// Stall cleared, test reset, owned job deleted — in a single reconcile pass.
		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		for _, c := range updated.Status.Conditions {
			if c.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				Expect(c.Status).To(Equal(corev1.ConditionFalse))
			}
		}
		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())

		// Second reconcile should be idempotent — no changes to stall/phase.
		_, err = f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))
	})

	It("survives back-to-back reconciles without re-stalling (idempotent)", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-5 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt.Add(-1*time.Minute), false)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		for i := 0; i < 3; i++ {
			_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
			Expect(err).NotTo(HaveOccurred())
		}

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		var stalled *kruiserolloutv1beta1.RolloutCondition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				stalled = &updated.Status.Conditions[i]
			}
		}
		Expect(stalled).NotTo(BeNil())
		Expect(stalled.Status).To(Equal(corev1.ConditionFalse))
	})

	It("skip mode: marks stale-failed tests as Skipped and keeps the Job", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-5 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt.Add(-1*time.Minute), true)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeSkip)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		// Test phase is Skipped (not WaitingForStep like the retry path).
		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSkipped))

		// Original failed Job is preserved (audit trail); retry mode would have deleted it.
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1))

		// Stalled condition cleared so the step can advance.
		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		for _, c := range updated.Status.Conditions {
			if c.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				Expect(c.Status).To(Equal(corev1.ConditionFalse))
			}
		}
	})

	It("skip mode: Skipped phase stays stable across reconciles (not restalled or reset)", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-5 * time.Minute)

		f.setStalled(ctx, "RolloutTestFailed", stallAt)
		f.setTestFailed(ctx, stallAt.Add(-1*time.Minute), false)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeSkip)

		for i := 0; i < 3; i++ {
			_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
			Expect(err).NotTo(HaveOccurred())
		}

		updatedTest := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updatedTest)).To(Succeed())
		Expect(updatedTest.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSkipped))

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		for _, c := range updated.Status.Conditions {
			if c.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
				Expect(c.Status).To(Equal(corev1.ConditionFalse))
			}
		}
	})
})

var _ = Describe("evaluateTests stale-failure guard", func() {
	It("returns not-failed when all failed tests predate retryCutoff", func() {
		r := &RolloutStepGateReconciler{}
		past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
		cutoff := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		tests := []rolloutv1alpha1.RolloutTest{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "t1"},
				Status: rolloutv1alpha1.RolloutTestStatus{
					Phase: rolloutv1alpha1.RolloutTestPhaseFailed,
					Conditions: []metav1.Condition{{
						Type: "Failed", Status: metav1.ConditionTrue, Reason: "JobFailed",
						LastTransitionTime: past,
					}},
				},
			},
		}
		allPassed, anyFailed, _ := r.evaluateTests(tests, &cutoff)
		Expect(anyFailed).To(BeFalse())
		Expect(allPassed).To(BeFalse())
	})

	It("still reports failed when test failure postdates retryCutoff", func() {
		r := &RolloutStepGateReconciler{}
		cutoff := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		recent := metav1.NewTime(time.Now().Add(-1 * time.Minute))
		tests := []rolloutv1alpha1.RolloutTest{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "t1"},
				Status: rolloutv1alpha1.RolloutTestStatus{
					Phase: rolloutv1alpha1.RolloutTestPhaseFailed,
					Conditions: []metav1.Condition{{
						Type: "Failed", Status: metav1.ConditionTrue, Reason: "JobFailed",
						LastTransitionTime: recent,
					}},
				},
			},
		}
		_, anyFailed, name := r.evaluateTests(tests, &cutoff)
		Expect(anyFailed).To(BeTrue())
		Expect(name).To(Equal("t1"))
	})

	It("reports failed when retryCutoff is nil (no retry recorded)", func() {
		r := &RolloutStepGateReconciler{}
		past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
		tests := []rolloutv1alpha1.RolloutTest{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "t1"},
				Status: rolloutv1alpha1.RolloutTestStatus{
					Phase: rolloutv1alpha1.RolloutTestPhaseFailed,
					Conditions: []metav1.Condition{{
						Type: "Failed", Status: metav1.ConditionTrue, Reason: "JobFailed",
						LastTransitionTime: past,
					}},
				},
			},
		}
		_, anyFailed, _ := r.evaluateTests(tests, nil)
		Expect(anyFailed).To(BeTrue())
	})

	It("treats Skipped phase as passed (like Succeeded)", func() {
		r := &RolloutStepGateReconciler{}
		tests := []rolloutv1alpha1.RolloutTest{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "t1"},
				Status:     rolloutv1alpha1.RolloutTestStatus{Phase: rolloutv1alpha1.RolloutTestPhaseSkipped},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "t2"},
				Status:     rolloutv1alpha1.RolloutTestStatus{Phase: rolloutv1alpha1.RolloutTestPhaseSucceeded},
			},
		}
		allPassed, anyFailed, _ := r.evaluateTests(tests, nil)
		Expect(allPassed).To(BeTrue())
		Expect(anyFailed).To(BeFalse())
	})
})

var _ = Describe("RolloutTest preserves WaitingForStep under stall", func() {
	ctx := context.Background()
	var namespace string
	var reconciler *RolloutTestReconciler
	var kruiseRollout *kruiserolloutv1beta1.Rollout
	var rolloutTest *rolloutv1alpha1.RolloutTest

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "retry-rt-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		kruiseRollout = &kruiserolloutv1beta1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
			Spec: kruiserolloutv1beta1.RolloutSpec{
				Strategy: kruiserolloutv1beta1.RolloutStrategy{
					Canary: &kruiserolloutv1beta1.CanaryStrategy{
						Steps: []kruiserolloutv1beta1.CanaryStep{{
							Replicas: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
							Pause:    kruiserolloutv1beta1.RolloutPause{Duration: func() *int32 { d := int32(3600); return &d }()},
						}},
					},
				},
				WorkloadRef: kruiserolloutv1beta1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "app-dep"},
			},
		}
		Expect(k8sClient.Create(ctx, kruiseRollout)).To(Succeed())
		kruiseRollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{CanaryRevision: "rev-1"}
		kruiseRollout.Status.CanaryStatus.CurrentStepIndex = 1
		kruiseRollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
		kruiseRollout.Status.CurrentStepIndex = 1
		kruiseRollout.Status.Conditions = []kruiserolloutv1beta1.RolloutCondition{{
			Type:               kruiserolloutv1beta1.RolloutConditionType("Stalled"),
			Status:             corev1.ConditionTrue,
			Reason:             "RolloutTestFailed",
			Message:            "stuck",
			LastTransitionTime: metav1.Now(),
			LastUpdateTime:     metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, kruiseRollout)).To(Succeed())

		rolloutTest = &rolloutv1alpha1.RolloutTest{
			ObjectMeta: metav1.ObjectMeta{Name: "app-test", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutTestSpec{
				RolloutName: "app",
				StepIndex:   1,
				JobTemplate: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "c", Image: "busybox", Command: []string{"true"}}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rolloutTest)).To(Succeed())
		rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
		Expect(k8sClient.Status().Update(ctx, rolloutTest)).To(Succeed())

		reconciler = &RolloutTestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	It("does not cancel a WaitingForStep test when the Kruise Rollout is stalled", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))
	})

	It("does not create a new job while stalled (WaitingForStep stays parked)", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})

	It("still cancels a Running test when stalled (original behavior preserved)", func() {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, rolloutTest)).To(Succeed())
		rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseRunning
		Expect(k8sClient.Status().Update(ctx, rolloutTest)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}})
		if err != nil && !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
	})
})
