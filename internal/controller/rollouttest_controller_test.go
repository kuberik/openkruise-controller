package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/openkruise-controller/api/v1alpha1"
	kruiserolloutv1beta1 "github.com/openkruise/kruise-rollout-api/rollouts/v1beta1"
)

var _ = Describe("RolloutTest Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		var namespace string
		var typeNamespacedName types.NamespacedName
		var rolloutTest *rolloutv1alpha1.RolloutTest
		var rollout *kruiserolloutv1beta1.Rollout

		BeforeEach(func() {
			ctx := context.Background()

			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: namespace,
			}

			By("creating the Rollout")
			rollout = &kruiserolloutv1beta1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: namespace,
				},
				Spec: kruiserolloutv1beta1.RolloutSpec{
					Strategy: kruiserolloutv1beta1.RolloutStrategy{
						Canary: &kruiserolloutv1beta1.CanaryStrategy{
							Steps: []kruiserolloutv1beta1.CanaryStep{
								{Replicas: &intstr.IntOrString{Type: intstr.Int, IntVal: 1}},
								{Replicas: &intstr.IntOrString{Type: intstr.Int, IntVal: 2}},
							},
						},
					},
					WorkloadRef: kruiserolloutv1beta1.ObjectRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "test-deployment",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("creating the RolloutTest")
			rolloutTest = &rolloutv1alpha1.RolloutTest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutTestSpec{
					RolloutName: "test-rollout",
					StepIndex:   1,
					JobTemplate: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "busybox", Command: []string{"echo", "hello"}},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rolloutTest)).To(Succeed())
		})

		AfterEach(func() {
			ctx := context.Background()
			By("Cleaning up the test namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			}
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		It("should handle successful rollout", func() {
			ctx := context.Background()
			controllerReconciler := &RolloutTestReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("Updating Rollout to step 1 and paused")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CurrentStepIndex = 1
			if rollout.Status.CanaryStatus == nil {
				rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
			}
			rollout.Status.CanaryStatus.CanaryRevision = "v1"
			rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling to create job")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Job was created")
			var jobs batchv1.JobList
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(1))
			job := &jobs.Items[0]

			By("Simulating Job success")
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobSuccessCriteriaMet,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "JobCompleted",
					Message:            "Job completed successfully",
				},
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "JobCompleted",
					Message:            "Job completed successfully",
				},
			}
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			By("Reconciling to update status")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying RolloutTest status shows success (kstatus Current)")
			var rt rolloutv1alpha1.RolloutTest
			err = k8sClient.Get(ctx, typeNamespacedName, &rt)
			Expect(err).NotTo(HaveOccurred())

			readyCondition := meta.FindStatusCondition(rt.Status.Conditions, "Ready")
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal("JobSucceeded"))

			result, err := status.Compute(convertToUnstructured(&rt))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(Equal(status.CurrentStatus))
		})

		It("should handle failed rollout", func() {
			ctx := context.Background()
			controllerReconciler := &RolloutTestReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("Updating Rollout to step 1 and paused")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CurrentStepIndex = 1
			if rollout.Status.CanaryStatus == nil {
				rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
			}
			rollout.Status.CanaryStatus.CanaryRevision = "v1"
			rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling to create job")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Job was created")
			var jobs batchv1.JobList
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(1))
			job := &jobs.Items[0]

			By("Simulating Job failure")
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobFailureTarget,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "BackoffLimitExceeded",
					Message:            "Job has reached the specified backoff limit",
				},
				{
					Type:               batchv1.JobFailed,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "BackoffLimitExceeded",
					Message:            "Job has reached the specified backoff limit",
				},
			}
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			By("Reconciling to update status")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying RolloutTest status shows failure (kstatus Failed)")
			var rt rolloutv1alpha1.RolloutTest
			err = k8sClient.Get(ctx, typeNamespacedName, &rt)
			Expect(err).NotTo(HaveOccurred())

			failedCondition := meta.FindStatusCondition(rt.Status.Conditions, "Failed")
			Expect(failedCondition).NotTo(BeNil())
			Expect(failedCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(failedCondition.Reason).To(Equal("BackoffLimitExceeded"))

			stalledCondition := meta.FindStatusCondition(rt.Status.Conditions, "Stalled")
			Expect(stalledCondition).NotTo(BeNil())
			Expect(stalledCondition.Status).To(Equal(metav1.ConditionTrue))

			result, err := status.Compute(convertToUnstructured(&rt))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(Equal(status.FailedStatus))
		})

		It("should transition from Failed to InProgress when new rollout starts", func() {
			ctx := context.Background()
			controllerReconciler := &RolloutTestReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Setup: Create a failed job for revision v1 using the controller
			By("Setting up initial state: Rollout at v1 and paused")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CurrentStepIndex = 1
			if rollout.Status.CanaryStatus == nil {
				rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
			}
			rollout.Status.CanaryStatus.CanaryRevision = "v1"
			rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling to create first job")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying first Job was created")
			var jobs batchv1.JobList
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(1))
			failedJob := &jobs.Items[0]

			By("Simulating first Job failure")
			now := metav1.Now()
			failedJob.Status.StartTime = &now
			failedJob.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobFailureTarget,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "BackoffLimitExceeded",
					Message:            "Job has reached the specified backoff limit",
				},
				{
					Type:               batchv1.JobFailed,
					Status:             corev1.ConditionTrue,
					LastProbeTime:      now,
					LastTransitionTime: now,
					Reason:             "BackoffLimitExceeded",
					Message:            "Job has reached the specified backoff limit",
				},
			}
			failedJob.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, failedJob)).To(Succeed())

			By("Reconciling to update status to Failed")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Verify it is failed before proceeding
			var rt rolloutv1alpha1.RolloutTest
			Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
			failedCondition := meta.FindStatusCondition(rt.Status.Conditions, "Failed")
			Expect(failedCondition).NotTo(BeNil())
			Expect(failedCondition.Status).To(Equal(metav1.ConditionTrue))

			By("Triggering new rollout (v2) and ensuring paused")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CanaryStatus.CanaryRevision = "v2"
			rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling - should delete old job")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying old job is deleted")
			// Manually reconcile until job is deleted
			// In envtest, deletion is async, so we might need to reconcile multiple times or wait
			// But since we are not using Eventually, we will use a loop with Reconcile
			for i := 0; i < 10; i++ {
				err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
				Expect(err).NotTo(HaveOccurred())
				if len(jobs.Items) == 0 {
					break
				}

				j := &jobs.Items[0]
				if !j.DeletionTimestamp.IsZero() && len(j.Finalizers) > 0 {
					// Manually remove finalizers to allow deletion in envtest
					j.Finalizers = []string{}
					_ = k8sClient.Update(ctx, j)
				}

				// Short sleep to allow envtest to process deletion
				// This is strictly for envtest async behavior, not testing logic
				// In real cluster, we would rely on Eventually or Watch
				// But user requested no Eventually.
				time.Sleep(100 * time.Millisecond)
			}

			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(0), "Old job should be deleted")

			By("Reconciling - should create new job")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying new job created")
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(1))
			Expect(jobs.Items[0].Status.Failed).To(Equal(int32(0)))
			Expect(jobs.Items[0].Status.Succeeded).To(Equal(int32(0)))

			By("Reconciling to update status")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status transitioned to InProgress")
			err = k8sClient.Get(ctx, typeNamespacedName, &rt)
			Expect(err).NotTo(HaveOccurred())

			// Stalled should be False
			stalledCondition := meta.FindStatusCondition(rt.Status.Conditions, "Stalled")
			Expect(stalledCondition).NotTo(BeNil())
			Expect(stalledCondition.Status).To(Equal(metav1.ConditionFalse))

			// kstatus should be InProgress
			result, err := status.Compute(convertToUnstructured(&rt))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(Equal(status.InProgressStatus))
		})

		It("should not create job when canary is not paused", func() {
			ctx := context.Background()
			controllerReconciler := &RolloutTestReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("Updating Rollout to step 1 but not paused")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CurrentStepIndex = 1
			if rollout.Status.CanaryStatus == nil {
				rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
			}
			rollout.Status.CanaryStatus.CanaryRevision = "v1"
			rollout.Status.CanaryStatus.CurrentStepState = "InProgress" // Not paused
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling - should not create job")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no Job was created")
			var jobs batchv1.JobList
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(0), "Job should not be created when canary is not paused")

			By("Updating Rollout to paused state")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
			rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling - should now create job")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Job was created after pause")
			err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(1), "Job should be created when canary is paused")
		})

		Context("When testing new status fields", func() {
			It("should set Phase to WaitingForStep when rollout hasn't reached the step", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 0 (before target step 1)")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 0
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is WaitingForStep")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))
				Expect(rt.Status.JobName).To(BeEmpty())
			})

			It("should set Phase to WaitingForStep when canary is not paused", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 1 but not paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 1
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "InProgress"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is WaitingForStep")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseWaitingForStep))
				Expect(rt.Status.JobName).To(BeEmpty())
			})

			It("should set Phase to Pending and JobName when job is created", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 1 and paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 1
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is Pending and JobName is set")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhasePending))
				Expect(rt.Status.JobName).NotTo(BeEmpty())
				Expect(rt.Status.RetryCount).To(Equal(int32(0)))
				Expect(rt.Status.ActivePods).To(Equal(int32(0)))
				Expect(rt.Status.SucceededPods).To(Equal(int32(0)))
				Expect(rt.Status.FailedPods).To(Equal(int32(0)))

				By("Verifying JobName matches actual job name")
				var jobs batchv1.JobList
				err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(jobs.Items).To(HaveLen(1))
				Expect(rt.Status.JobName).To(Equal(jobs.Items[0].Name))
			})

			It("should set Phase to Running and update pod counts when job starts", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 1 and paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 1
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Getting the created job")
				var jobs batchv1.JobList
				err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(jobs.Items).To(HaveLen(1))
				job := &jobs.Items[0]

				By("Simulating job start with active pods")
				now := metav1.Now()
				job.Status.StartTime = &now
				job.Status.Active = 1
				job.Status.Succeeded = 0
				job.Status.Failed = 0
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is Running and pod counts are updated")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseRunning))
				Expect(rt.Status.ActivePods).To(Equal(int32(1)))
				Expect(rt.Status.SucceededPods).To(Equal(int32(0)))
				Expect(rt.Status.FailedPods).To(Equal(int32(0)))
				Expect(rt.Status.RetryCount).To(Equal(int32(0)))
			})

			It("should set Phase to Succeeded and update pod counts when job succeeds", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 1 and paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 1
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Getting the created job")
				var jobs batchv1.JobList
				err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(jobs.Items).To(HaveLen(1))
				job := &jobs.Items[0]

				By("Simulating job success")
				now := metav1.Now()
				job.Status.StartTime = &now
				job.Status.CompletionTime = &now
				job.Status.Succeeded = 1
				job.Status.Active = 0
				job.Status.Failed = 0
				job.Status.Conditions = []batchv1.JobCondition{
					{
						Type:               batchv1.JobSuccessCriteriaMet,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
					},
					{
						Type:               batchv1.JobComplete,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
					},
				}
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is Succeeded and pod counts are updated")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSucceeded))
				Expect(rt.Status.SucceededPods).To(Equal(int32(1)))
				Expect(rt.Status.ActivePods).To(Equal(int32(0)))
				Expect(rt.Status.FailedPods).To(Equal(int32(0)))
				Expect(rt.Status.RetryCount).To(Equal(int32(0)))
			})

			It("should set Phase to Failed and update RetryCount and pod counts when job fails", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting Rollout to step 1 and paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 1
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Getting the created job")
				var jobs batchv1.JobList
				err = k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(jobs.Items).To(HaveLen(1))
				job := &jobs.Items[0]

				By("Simulating job failure with retries")
				now := metav1.Now()
				job.Status.StartTime = &now
				job.Status.Failed = 3 // 3 failed pods (retries)
				job.Status.Active = 0
				job.Status.Succeeded = 0
				job.Status.Conditions = []batchv1.JobCondition{
					{
						Type:               batchv1.JobFailureTarget,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "BackoffLimitExceeded",
						Message:            "Job has reached the specified backoff limit",
					},
					{
						Type:               batchv1.JobFailed,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "BackoffLimitExceeded",
						Message:            "Job has reached the specified backoff limit",
					},
				}
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Phase is Failed and counts are updated")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseFailed))
				Expect(rt.Status.FailedPods).To(Equal(int32(3)))
				Expect(rt.Status.RetryCount).To(Equal(int32(3))) // RetryCount should equal FailedPods
				Expect(rt.Status.ActivePods).To(Equal(int32(0)))
				Expect(rt.Status.SucceededPods).To(Equal(int32(0)))
			})
		})

		Context("When step is manually approved", func() {
			It("should cancel the job when CurrentStepIndex moves forward", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting up rollout at step 1, paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CurrentStepIndex = 1
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create the job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying job was created")
				var job batchv1.Job
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})).To(Succeed())
				Expect(jobs.Items).To(HaveLen(1))
				job = jobs.Items[0]

				By("Setting job to running state")
				job.Status.Active = 1
				job.Status.StartTime = &metav1.Time{Time: time.Now()}
				Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying RolloutTest is in Running phase")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseRunning))
				Expect(rt.Status.JobName).To(Equal(job.Name))

				By("Manually approving step by moving to step 2")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 2
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling - should detect step change and cancel job")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying job was deleted")
				// In envtest, deletion is async, so we might need to handle finalizers
				for i := 0; i < 10; i++ {
					var deletedJob batchv1.Job
					err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
					if errors.IsNotFound(err) {
						break
					}
					if !deletedJob.DeletionTimestamp.IsZero() && len(deletedJob.Finalizers) > 0 {
						deletedJob.Finalizers = []string{}
						_ = k8sClient.Update(ctx, &deletedJob)
					}
					time.Sleep(100 * time.Millisecond)
				}
				var deletedJob batchv1.Job
				err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				By("Verifying RolloutTest status is set to Cancelled")
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseCancelled))
				Expect(rt.Status.JobName).To(BeEmpty())

				By("Verifying conditions are updated")
				readyCondition := meta.FindStatusCondition(rt.Status.Conditions, "Ready")
				Expect(readyCondition).NotTo(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
				Expect(readyCondition.Reason).To(Equal("JobCancelled"))
				Expect(readyCondition.Message).To(ContainSubstring("cancelled because rollout step moved forward"))

				failedCondition := meta.FindStatusCondition(rt.Status.Conditions, "Failed")
				Expect(failedCondition).NotTo(BeNil())
				Expect(failedCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(failedCondition.Reason).To(Equal("JobCancelled"))

				stalledCondition := meta.FindStatusCondition(rt.Status.Conditions, "Stalled")
				Expect(stalledCondition).NotTo(BeNil())
				Expect(stalledCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(stalledCondition.Reason).To(Equal("JobCancelled"))
			})

			It("should NOT mark succeeded test as Cancelled when step moves forward", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting up rollout at step 1, paused")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CurrentStepIndex = 1
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create the job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Getting the created job")
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})).To(Succeed())
				Expect(jobs.Items).To(HaveLen(1))
				job := jobs.Items[0]

				By("Setting job to succeeded state")
				now := metav1.Now()
				job.Status.StartTime = &now
				job.Status.CompletionTime = &now
				job.Status.Succeeded = 1
				job.Status.Conditions = []batchv1.JobCondition{
					{
						Type:               batchv1.JobComplete,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "JobCompleted",
						Message:            "Job completed successfully",
					},
					{
						Type:               batchv1.JobSuccessCriteriaMet,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "JobCompleted",
						Message:            "Job completed successfully",
					},
				}
				Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

				By("Reconciling to update status to Succeeded")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying RolloutTest is in Succeeded phase")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSucceeded))

				By("Manually approving step by moving to step 2")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CurrentStepIndex = 2
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling - should delete job but preserve Succeeded phase")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying job was deleted")
				// In envtest, deletion is async, so we might need to handle finalizers
				for i := 0; i < 10; i++ {
					var deletedJob batchv1.Job
					err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
					if errors.IsNotFound(err) {
						break
					}
					if !deletedJob.DeletionTimestamp.IsZero() && len(deletedJob.Finalizers) > 0 {
						deletedJob.Finalizers = []string{}
						_ = k8sClient.Update(ctx, &deletedJob)
					}
					time.Sleep(100 * time.Millisecond)
				}
				var deletedJob batchv1.Job
				err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				By("Verifying RolloutTest phase is preserved as Succeeded (not changed to Cancelled)")
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.Phase).To(Equal(rolloutv1alpha1.RolloutTestPhaseSucceeded))
				// JobName should still be set (preserved from previous status)
				Expect(rt.Status.JobName).To(Equal(job.Name))
			})
		})

		Context("When new canary is observed", func() {
			It("should reset RetryCount and FailedPods when canaryRevision changes", func() {
				ctx := context.Background()
				controllerReconciler := &RolloutTestReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				By("Setting up rollout at step 1, paused, with canary v1")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				if rollout.Status.CanaryStatus == nil {
					rollout.Status.CanaryStatus = &kruiserolloutv1beta1.CanaryStatus{}
				}
				rollout.Status.CurrentStepIndex = 1
				rollout.Status.CanaryStatus.CanaryRevision = "v1"
				rollout.Status.CanaryStatus.CurrentStepState = "StepPaused"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling to create the job")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Getting the created job")
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": resourceName})).To(Succeed())
				Expect(jobs.Items).To(HaveLen(1))
				job := jobs.Items[0]

				By("Setting job to failed state with retries")
				now := metav1.Now()
				job.Status.Failed = 3
				job.Status.Active = 0
				job.Status.StartTime = &now
				job.Status.Conditions = []batchv1.JobCondition{
					{
						Type:               batchv1.JobFailureTarget,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "BackoffLimitExceeded",
						Message:            "Job has reached the specified backoff limit",
					},
					{
						Type:               batchv1.JobFailed,
						Status:             corev1.ConditionTrue,
						LastProbeTime:      now,
						LastTransitionTime: now,
						Reason:             "BackoffLimitExceeded",
						Message:            "Job has reached the specified backoff limit",
					},
				}
				Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying RolloutTest has RetryCount and FailedPods set")
				var rt rolloutv1alpha1.RolloutTest
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.RetryCount).To(Equal(int32(3)))
				Expect(rt.Status.FailedPods).To(Equal(int32(3)))
				Expect(rt.Status.ObservedCanaryRevision).To(Equal("v1"))

				By("Changing canaryRevision to v2")
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-rollout", Namespace: namespace}, rollout)).To(Succeed())
				rollout.Status.CanaryStatus.CanaryRevision = "v2"
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Reconciling - should detect canary change, delete old job, and reset status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying old job was deleted")
				// In envtest, deletion is async, so we might need to handle finalizers
				for i := 0; i < 10; i++ {
					var deletedJob batchv1.Job
					err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
					if errors.IsNotFound(err) {
						break
					}
					if !deletedJob.DeletionTimestamp.IsZero() && len(deletedJob.Finalizers) > 0 {
						deletedJob.Finalizers = []string{}
						_ = k8sClient.Update(ctx, &deletedJob)
					}
					time.Sleep(100 * time.Millisecond)
				}
				var deletedJob batchv1.Job
				err = k8sClient.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &deletedJob)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				By("Verifying RetryCount and FailedPods are reset")
				Expect(k8sClient.Get(ctx, typeNamespacedName, &rt)).To(Succeed())
				Expect(rt.Status.RetryCount).To(Equal(int32(0)))
				Expect(rt.Status.FailedPods).To(Equal(int32(0)))
				Expect(rt.Status.ActivePods).To(Equal(int32(0)))
				Expect(rt.Status.SucceededPods).To(Equal(int32(0)))
				Expect(rt.Status.JobName).To(BeEmpty())
			})
		})
	})
})
