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
	testReconciler *RolloutTestReconciler
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
	// rollouttest controller checks the top-level Status.CurrentStepIndex
	// (separate from CanaryStatus.CurrentStepIndex); set both for parity.
	kruiseRollout.Status.CurrentStepIndex = 1
	kruiseRollout.Status.CurrentStepState = "StepPaused"
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
		testReconciler: &RolloutTestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()},
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

	It("skip mode: marks stale-failed tests as Skipped and deletes the Job", func() {
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

		// The owned Job is deleted in both modes: keeping it around lets the
		// rollouttest controller's updateStatus re-sync the old job's state
		// onto our Skipped patch (e.g. BackoffLimitExceeded → phase=Failed).
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())

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

var _ = Describe("RolloutTest terminal-phase job-creation guard", func() {
	ctx := context.Background()
	var namespace string
	var reconciler *RolloutTestReconciler
	var kruiseRollout *kruiserolloutv1beta1.Rollout
	var rolloutTest *rolloutv1alpha1.RolloutTest

	// Rollout sits at step 1, unstalled, canary paused — the exact conditions
	// where the "create Job" branch would fire if the terminal-phase guard
	// didn't short-circuit first.
	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "term-guard-ns-"}}
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
		kruiseRollout.Status.CurrentStepIndex = 1
		kruiseRollout.Status.CurrentStepState = "StepPaused"
		kruiseRollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{CanaryRevision: "rev-1"}
		kruiseRollout.Status.CanaryStatus.CurrentStepIndex = 1
		kruiseRollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
		// Deliberately no Stalled condition — the guard under test is the
		// terminal-phase one, not the stalled-reasons one.
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

		reconciler = &RolloutTestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	setPhaseAndReconcile := func(phase rolloutv1alpha1.RolloutTestPhase) {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, rolloutTest)).To(Succeed())
		rolloutTest.Status.Phase = phase
		rolloutTest.Status.ObservedCanaryRevision = "rev-1"
		Expect(k8sClient.Status().Update(ctx, rolloutTest)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())
	}

	It("does not create a Job when phase is Failed", func() {
		setPhaseAndReconcile(rolloutv1alpha1.RolloutTestPhaseFailed)

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})

	It("does not create a Job when phase is Cancelled (prevents race with stepgate Skipped marking)", func() {
		setPhaseAndReconcile(rolloutv1alpha1.RolloutTestPhaseCancelled)

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())

		// Phase stays Cancelled — no overwrite to Pending.
		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
	})

	It("does not create a Job when phase is Skipped (skip-mode outcome must be sticky)", func() {
		setPhaseAndReconcile(rolloutv1alpha1.RolloutTestPhaseSkipped)

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rolloutTest.Name, Namespace: namespace}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSkipped))
	})

	It("does not create a Job when phase is Succeeded", func() {
		setPhaseAndReconcile(rolloutv1alpha1.RolloutTestPhaseSucceeded)

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})

	It("DOES create a Job when phase is WaitingForStep (retry-mode reset path)", func() {
		setPhaseAndReconcile(rolloutv1alpha1.RolloutTestPhaseWaitingForStep)

		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1), "WaitingForStep is the non-terminal retry reset — a fresh Job should be created")
	})
})

var _ = Describe("RolloutStepGate step deadline refresh on retry", func() {
	ctx := context.Background()
	var f *retryFixture

	// stepStartedAtKey / stepReadyAtKey are duplicated from the controller so
	// tests don't couple to its unexported prefix formatting beyond the key
	// strings they assert.
	const stepStartedAtKey = "internal.rollout.kuberik.io/step-1-started-at"
	const stepReadyAtKey = "internal.rollout.kuberik.io/step-1-ready-at"

	BeforeEach(func() {
		f = newRetryFixture(ctx)
	})
	AfterEach(func() {
		f.cleanup(ctx)
	})

	// seedStaleDeadline puts step-started-at in the past so its deadline is
	// already exceeded — the exact situation that caused re-stall before
	// Fix A: stepgate clears Stalled, next reconcile sees "deadline exceeded",
	// re-stalls with StepReadyTimeoutExceeded.
	seedStaleDeadline := func(ctx context.Context, stalePast time.Time) {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, f.kruiseRollout)).To(Succeed())
		if f.kruiseRollout.Annotations == nil {
			f.kruiseRollout.Annotations = map[string]string{}
		}
		f.kruiseRollout.Annotations[stepStartedAtKey] = stalePast.Format(time.RFC3339)
		f.kruiseRollout.Annotations[stepReadyAtKey] = stalePast.Add(30 * time.Second).Format(time.RFC3339)
		f.kruiseRollout.Annotations["rollout.kuberik.io/step-1-ready-timeout"] = "1m"
		f.kruiseRollout.Annotations["internal.rollout.kuberik.io/last-step-index"] = "1"
		Expect(k8sClient.Update(ctx, f.kruiseRollout)).To(Succeed())
	}

	It("refreshes step-started-at to retryCutoff and clears step-ready-at", func() {
		stalePast := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-5 * time.Minute)

		seedStaleDeadline(ctx, stalePast)
		f.setStalled(ctx, "RolloutTestFailed", stalePast.Add(1*time.Minute))
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Annotations[stepStartedAtKey]).To(Equal(retryAt.Format(time.RFC3339)))
		Expect(updated.Annotations).NotTo(HaveKey(stepReadyAtKey))
	})

	It("doesn't touch deadline when retry cutoff is absent", func() {
		stalePast := time.Now().Add(-30 * time.Minute)
		stallAt := time.Now().Add(-1 * time.Minute)

		seedStaleDeadline(ctx, stalePast)
		f.setStalled(ctx, "StepReadyTimeoutExceeded", stallAt)
		// No setKuberikRetry — no retry recorded.

		before := stalePast.Format(time.RFC3339)
		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Annotations[stepStartedAtKey]).To(Equal(before), "step-started-at should not be refreshed without a retry cutoff")
	})

	It("back-to-back retries advance step-started-at each time", func() {
		stalePast := time.Now().Add(-30 * time.Minute)
		firstRetry := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
		secondRetry := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

		seedStaleDeadline(ctx, stalePast)
		f.setStalled(ctx, "RolloutTestFailed", stalePast.Add(1*time.Minute))
		f.setKuberikRetry(ctx, firstRetry, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		first := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, first)).To(Succeed())
		Expect(first.Annotations[stepStartedAtKey]).To(Equal(firstRetry.Format(time.RFC3339)))

		// Simulate a fresh stall after first retry, then another retry with
		// newer cutoff.
		f.setStalled(ctx, "RolloutTestFailed", firstRetry.Add(10*time.Second))
		f.setKuberikRetry(ctx, secondRetry, kuberikrolloutv1alpha1.RetryModeRetry)

		_, err = f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		second := &kruiserolloutv1beta1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.kruiseRollout.Name, Namespace: f.ns}, second)).To(Succeed())
		Expect(second.Annotations[stepStartedAtKey]).To(Equal(secondRetry.Format(time.RFC3339)))
	})
})

var _ = Describe("RolloutTest stale-Stalled guard (retry cutoff)", func() {
	ctx := context.Background()
	var f *retryFixture

	BeforeEach(func() {
		f = newRetryFixture(ctx)
	})
	AfterEach(func() {
		f.cleanup(ctx)
	})

	// markTestPhase writes a non-terminal phase so the rollouttest controller's
	// "stalled → cancel" branch has something to actually cancel. Without this
	// the controller's early-exit for terminal phases would hide the guard.
	markTestPhase := func(ctx context.Context, phase rolloutv1alpha1.RolloutTestPhase) {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, f.rolloutTest)).To(Succeed())
		f.rolloutTest.Status.Phase = phase
		f.rolloutTest.Status.ObservedCanaryRevision = "rev-1"
		Expect(k8sClient.Status().Update(ctx, f.rolloutTest)).To(Succeed())
	}

	It("does NOT cancel a Pending test when Stalled predates the retry cutoff", func() {
		// Stalled set 30 min ago (pre-retry), retry 1 min ago. The stall is
		// stale — stepgate is unwinding. The rollouttest controller should
		// hold off, not cancel.
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-1 * time.Minute)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)
		markTestPhase(ctx, rolloutv1alpha1.RolloutTestPhasePending)

		result, err := f.testReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue while waiting for stepgate to clear stale stall")

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhasePending), "phase must not flip to Cancelled on stale stall")
	})

	It("DOES cancel a Pending test when Stalled is fresh (postdates retry cutoff)", func() {
		// Retry was a while ago, stall happened just now — this is a genuine
		// post-retry failure. The guard should not apply; normal cancel runs.
		retryAt := time.Now().Add(-30 * time.Minute)
		stallAt := time.Now().Add(-5 * time.Second)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)
		markTestPhase(ctx, rolloutv1alpha1.RolloutTestPhasePending)

		_, err := f.testReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
	})

	It("cancels normally when no retry cutoff is recorded", func() {
		stallAt := time.Now().Add(-10 * time.Minute)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		// no setKuberikRetry → no cutoff → guard is inert
		markTestPhase(ctx, rolloutv1alpha1.RolloutTestPhasePending)

		_, err := f.testReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
	})

	It("does NOT delete an existing Job when Stalled predates retry cutoff (job-exists branch)", func() {
		stallAt := time.Now().Add(-30 * time.Minute)
		retryAt := time.Now().Add(-1 * time.Minute)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		// Create a Job owned by the test with the current canary revision label.
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-test-job",
				Namespace: f.ns,
				Labels: map[string]string{
					"rollout-test":                         f.rolloutTest.Name,
					"rollout.kuberik.io/canary-revision":   "rev-1",
				},
			},
			Spec: f.rolloutTest.Spec.JobTemplate,
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, f.rolloutTest)).To(Succeed())
		f.rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseRunning
		f.rolloutTest.Status.ObservedCanaryRevision = "rev-1"
		f.rolloutTest.Status.JobName = job.Name
		Expect(k8sClient.Status().Update(ctx, f.rolloutTest)).To(Succeed())

		_, err := f.testReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		// Job should still exist — stale stall shouldn't trigger deletion.
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name})).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1))

		// Phase should NOT be Cancelled (the guard prevented the cancel branch).
		// The rollouttest controller's normal updateStatus path will derive the
		// phase from the Job — Pending or Running — depending on whether the job
		// has started. Either is correct.
		updated := &rolloutv1alpha1.RolloutTest{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, updated)).To(Succeed())
		Expect(updated.Status.Phase).NotTo(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
	})

	It("DOES delete an existing Job when Stalled is fresh (job-exists branch)", func() {
		retryAt := time.Now().Add(-30 * time.Minute)
		stallAt := time.Now().Add(-5 * time.Second)

		f.setStalled(ctx, "KuberikRolloutBakeFailed", stallAt)
		f.setKuberikRetry(ctx, retryAt, kuberikrolloutv1alpha1.RetryModeRetry)

		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-test-job-fresh",
				Namespace: f.ns,
				Labels: map[string]string{
					"rollout-test":                         f.rolloutTest.Name,
					"rollout.kuberik.io/canary-revision":   "rev-1",
				},
			},
			Spec: f.rolloutTest.Spec.JobTemplate,
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}, f.rolloutTest)).To(Succeed())
		f.rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseRunning
		f.rolloutTest.Status.ObservedCanaryRevision = "rev-1"
		f.rolloutTest.Status.JobName = job.Name
		Expect(k8sClient.Status().Update(ctx, f.rolloutTest)).To(Succeed())

		_, err := f.testReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.rolloutTest.Name, Namespace: f.ns}})
		Expect(err).NotTo(HaveOccurred())

		// Fresh stall → job should be deleted (original behavior).
		Eventually(func() int {
			var jobs batchv1.JobList
			if err := k8sClient.List(ctx, &jobs, client.InNamespace(f.ns), client.MatchingLabels{"rollout-test": f.rolloutTest.Name}); err != nil {
				return -1
			}
			return len(jobs.Items)
		}).Should(Equal(0))
	})
})
