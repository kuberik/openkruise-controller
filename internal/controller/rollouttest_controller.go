/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/openkruise-controller/api/v1alpha1"
	kruiserolloutv1beta1 "github.com/openkruise/kruise-rollout-api/rollouts/v1beta1"
)

// RolloutTestReconciler reconciles a RolloutTest object
type RolloutTestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests/finalizers,verbs=update
// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RolloutTestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rolloutTest rolloutv1alpha1.RolloutTest
	if err := r.Get(ctx, req.NamespacedName, &rolloutTest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// List Jobs owned by this RolloutTest
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(req.Namespace), client.MatchingLabels{"rollout-test": req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	var job *batchv1.Job
	if len(jobs.Items) > 0 {
		job = &jobs.Items[0]
		// Refetch the job to get the latest status
		var latestJob batchv1.Job
		if err := r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &latestJob); err == nil {
			job = &latestJob
		}
	}

	// Fetch the Rollout to check current state
	var rollout kruiserolloutv1beta1.Rollout
	if err := r.Get(ctx, types.NamespacedName{Name: rolloutTest.Spec.RolloutName, Namespace: rolloutTest.Namespace}, &rollout); err != nil {
		if errors.IsNotFound(err) {
			// Rollout not found, wait
			log.Info("Rollout not found", "rolloutName", rolloutTest.Spec.RolloutName)
			return ctrl.Result{}, nil // Retry when Rollout is created (via Watch)
		}
		log.Error(err, "unable to fetch Rollout")
		return ctrl.Result{}, err
	}

	// If Job exists, check if the canary revision has changed or step has moved forward
	if job != nil {
		// Check the job's revision label
		jobRevision := job.Labels["rollout.kuberik.io/canary-revision"]

		// Check if rollout has moved to a different step (manually approved) or is stalled
		// If CurrentStepIndex is no longer at the RolloutTest's StepIndex, or Rollout is Stalled, cancel the job
		isStalled, stallReason := r.isRolloutStalled(&rollout)
		// If stalled due to TestFailed, do NOT cancel the job - let it be reported as Failed
		shouldCancel := isStalled && stallReason != "RolloutTestFailed"

		if rollout.Status.CurrentStepIndex != rolloutTest.Spec.StepIndex || shouldCancel {
			cancellationMessage := "Test job was cancelled because rollout step moved forward"
			if shouldCancel {
				log.Info("Rollout is stalled (non-test reason), cancelling job", "reason", stallReason)
				cancellationMessage = fmt.Sprintf("Test job was cancelled because rollout is stalled (%s)", stallReason)
			} else {
				log.Info("Rollout step changed, cancelling job",
					"currentStep", rollout.Status.CurrentStepIndex,
					"testStep", rolloutTest.Spec.StepIndex)
			}
			// If job finished checking, update status before deletion to preserve Succeeded/Failed state
			if r.isJobSucceeded(job) || r.isJobFailed(job) {
				currentRevision := ""
				if rollout.Status.CanaryStatus != nil {
					currentRevision = rollout.Status.CanaryStatus.CanaryRevision
				}
				// Update status to preserve terminal state (Succeeded/Failed)
				// The cache is automatically updated by controller-runtime
				_, _ = r.updateStatus(ctx, &rolloutTest, job, currentRevision)
			}

			if err := r.Delete(ctx, job); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "failed to delete job")
				return ctrl.Result{}, err
			}
			// Only update status if test was still running or pending
			// If test already succeeded or failed, preserve that status until new canary revision
			if rolloutTest.Status.Phase == rolloutv1alpha1.RolloutTestPhaseRunning ||
				rolloutTest.Status.Phase == rolloutv1alpha1.RolloutTestPhasePending {
				// Reset status fields and set phase to Cancelled
				rolloutTest.Status.JobName = ""
				rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseCancelled
				rolloutTest.Status.RetryCount = 0
				rolloutTest.Status.ActivePods = 0
				rolloutTest.Status.SucceededPods = 0
				rolloutTest.Status.FailedPods = 0

				// Update conditions to reflect cancellation
				// Ready is True because cancellation is a final state - nothing more to do
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            cancellationMessage,
					LastTransitionTime: metav1.Now(),
				})
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Failed",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            cancellationMessage,
					LastTransitionTime: metav1.Now(),
				})
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Stalled",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            cancellationMessage,
					LastTransitionTime: metav1.Now(),
				})

				if err := r.Status().Update(ctx, &rolloutTest); err != nil {
					log.Error(err, "failed to update status after job cancellation")
					return ctrl.Result{}, err
				}
			} else {
				// Test is in terminal state (Succeeded/Failed), preserve phase but clear Stalled condition
				// The step has moved forward, so the test is no longer blocking progress
				stalledCondition := meta.FindStatusCondition(rolloutTest.Status.Conditions, "Stalled")
				if stalledCondition != nil && stalledCondition.Status == metav1.ConditionTrue {
					meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
						Type:               "Ready",
						Status:             metav1.ConditionTrue,
						ObservedGeneration: rolloutTest.Generation,
						Reason:             "StepAdvanced",
						Message:            "Step moved forward, test result is final",
						LastTransitionTime: metav1.Now(),
					})
					meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
						Type:               "Stalled",
						Status:             metav1.ConditionFalse,
						ObservedGeneration: rolloutTest.Generation,
						Reason:             "StepAdvanced",
						Message:            "Step moved forward, test is no longer blocking progress",
						LastTransitionTime: metav1.Now(),
					})
					if err := r.Status().Update(ctx, &rolloutTest); err != nil {
						log.Error(err, "failed to clear Stalled condition after step advance")
						return ctrl.Result{}, err
					}
				}
			}
			// Test results will be reset when new canary revision is observed
			return ctrl.Result{}, nil
		}

		// If the rollout's canaryRevision is different from what the job is labeled with, the rollout has changed
		// Delete the old job so a new one can be created for the new rollout
		if rollout.Status.CanaryStatus != nil &&
			rollout.Status.CanaryStatus.CanaryRevision != "" &&
			jobRevision != rollout.Status.CanaryStatus.CanaryRevision {

			log.Info("Rollout canaryRevision changed, deleting old job",
				"oldRevision", jobRevision,
				"newRevision", rollout.Status.CanaryStatus.CanaryRevision)
			if err := r.Delete(ctx, job); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "failed to delete old job")
				return ctrl.Result{}, err
			}
			// Reset status fields when new canary is observed
			rolloutTest.Status.JobName = ""
			rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
			rolloutTest.Status.RetryCount = 0
			rolloutTest.Status.ActivePods = 0
			rolloutTest.Status.SucceededPods = 0
			rolloutTest.Status.FailedPods = 0
			rolloutTest.Status.ObservedCanaryRevision = ""
			if err := r.Status().Update(ctx, &rolloutTest); err != nil {
				log.Error(err, "failed to update status after canary change")
				return ctrl.Result{}, err
			}
			// Job deleted, will be recreated when rollout reaches target step
			return ctrl.Result{}, nil
		}

		// If Job exists and matches revision (or we can't tell), update status based on job
		// Get the current canaryRevision (may be empty if CanaryStatus is nil)
		currentRevision := ""
		if rollout.Status.CanaryStatus != nil {
			currentRevision = rollout.Status.CanaryStatus.CanaryRevision
		}
		return r.updateStatus(ctx, &rolloutTest, job, currentRevision)
	}

	// If Job does not exist, check if we should create one
	// First, if step has moved forward and test is in terminal state with Stalled=True, clear Stalled
	if rollout.Status.CurrentStepIndex != rolloutTest.Spec.StepIndex &&
		(rolloutTest.Status.Phase == rolloutv1alpha1.RolloutTestPhaseSucceeded ||
			rolloutTest.Status.Phase == rolloutv1alpha1.RolloutTestPhaseFailed) {
		stalledCondition := meta.FindStatusCondition(rolloutTest.Status.Conditions, "Stalled")
		if stalledCondition != nil && stalledCondition.Status == metav1.ConditionTrue {
			log.Info("Step advanced past terminal test, clearing Stalled condition",
				"currentStep", rollout.Status.CurrentStepIndex,
				"testStep", rolloutTest.Spec.StepIndex,
				"phase", rolloutTest.Status.Phase)
			meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: rolloutTest.Generation,
				Reason:             "StepAdvanced",
				Message:            "Step moved forward, test result is final",
				LastTransitionTime: metav1.Now(),
			})
			meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
				Type:               "Stalled",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: rolloutTest.Generation,
				Reason:             "StepAdvanced",
				Message:            "Step moved forward, test is no longer blocking progress",
				LastTransitionTime: metav1.Now(),
			})
			if err := r.Status().Update(ctx, &rolloutTest); err != nil {
				log.Error(err, "failed to clear Stalled condition after step advance (no job)")
				return ctrl.Result{}, err
			}
		}
	}

	// Now check if the canary revision has changed - if so, reset status
	currentRevision := ""
	if rollout.Status.CanaryStatus != nil {
		currentRevision = rollout.Status.CanaryStatus.CanaryRevision
	}

	// If canary revision changed since last observation, reset the test status
	if rolloutTest.Status.ObservedCanaryRevision != "" &&
		currentRevision != "" &&
		rolloutTest.Status.ObservedCanaryRevision != currentRevision {
		log.Info("Canary revision changed, resetting test status",
			"oldRevision", rolloutTest.Status.ObservedCanaryRevision,
			"newRevision", currentRevision)
		rolloutTest.Status.JobName = ""
		rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
		rolloutTest.Status.RetryCount = 0
		rolloutTest.Status.ActivePods = 0
		rolloutTest.Status.SucceededPods = 0
		rolloutTest.Status.FailedPods = 0
		rolloutTest.Status.ObservedCanaryRevision = ""
		// Clear conditions
		rolloutTest.Status.Conditions = []metav1.Condition{}
		if err := r.Status().Update(ctx, &rolloutTest); err != nil {
			log.Error(err, "failed to reset status after canary change")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// We trigger if CurrentStepIndex matches StepIndex AND canary is paused
	if rollout.Status.CurrentStepIndex == rolloutTest.Spec.StepIndex {
		// Check if rollout is stalled
		isStalled, stallReason := r.isRolloutStalled(&rollout)
		// If stalled for any reason, don't create new jobs
		if isStalled {
			log.Info("Rollout is stalled, not creating Job", "step", rolloutTest.Spec.StepIndex, "reason", stallReason)
			// Only cancel if not already in a terminal state (Succeeded, Failed, or Cancelled)
			if rolloutTest.Status.Phase != rolloutv1alpha1.RolloutTestPhaseCancelled &&
				rolloutTest.Status.Phase != rolloutv1alpha1.RolloutTestPhaseSucceeded &&
				rolloutTest.Status.Phase != rolloutv1alpha1.RolloutTestPhaseFailed {
				rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseCancelled
				rolloutTest.Status.JobName = ""

				message := "Test job creation skipped because rollout is stalled"
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            message,
					LastTransitionTime: metav1.Now(),
				})
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Failed",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            message,
					LastTransitionTime: metav1.Now(),
				})
				meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
					Type:               "Stalled",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: rolloutTest.Generation,
					Reason:             "JobCancelled",
					Message:            message,
					LastTransitionTime: metav1.Now(),
				})

				if err := r.Status().Update(ctx, &rolloutTest); err != nil {
					log.Error(err, "failed to update status")
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		// Don't auto-retry if test already failed - wait for user to continue rollout
		// This prevents a race where the rolloutstepgate hasn't set Stalled yet on the Kruise rollout
		if rolloutTest.Status.Phase == rolloutv1alpha1.RolloutTestPhaseFailed {
			log.Info("Test already failed, waiting for manual intervention", "step", rolloutTest.Spec.StepIndex)
			return ctrl.Result{}, nil
		}
		// Check if canary is in paused state before creating the job
		if !r.isCanaryPaused(&rollout) {
			log.Info("Rollout is at target step but canary is not paused, waiting", "step", rolloutTest.Spec.StepIndex)
			// Set phase to WaitingForStep
			rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
			rolloutTest.Status.JobName = ""
			rolloutTest.Status.RetryCount = 0
			rolloutTest.Status.ActivePods = 0
			rolloutTest.Status.SucceededPods = 0
			rolloutTest.Status.FailedPods = 0
			if err := r.Status().Update(ctx, &rolloutTest); err != nil {
				log.Error(err, "failed to update status")
			}
			return ctrl.Result{}, nil
		}
		log.Info("Rollout is at target step and canary is paused, creating Job", "step", rolloutTest.Spec.StepIndex)
		// currentRevision already declared above
		return r.createJob(ctx, &rolloutTest, currentRevision)
	}

	// Not at the step yet - set phase to WaitingForStep
	if rollout.Status.CurrentStepIndex < rolloutTest.Spec.StepIndex {
		rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
		rolloutTest.Status.JobName = ""
		if err := r.Status().Update(ctx, &rolloutTest); err != nil {
			log.Error(err, "failed to update status")
		}
	}
	return ctrl.Result{}, nil
}

func (r *RolloutTestReconciler) createJob(ctx context.Context, rolloutTest *rolloutv1alpha1.RolloutTest, canaryRevision string) (ctrl.Result, error) {
	labels := map[string]string{
		"rollout-test": rolloutTest.Name,
	}
	if canaryRevision != "" {
		labels["rollout.kuberik.io/canary-revision"] = canaryRevision
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: rolloutTest.Name + "-",
			Namespace:    rolloutTest.Namespace,
			Labels:       labels,
		},
		Spec: rolloutTest.Spec.JobTemplate,
	}

	if err := ctrl.SetControllerReference(rolloutTest, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, err
	}

	// Update the RolloutTest status to record which canaryRevision this job is for
	rolloutTest.Status.ObservedCanaryRevision = canaryRevision
	rolloutTest.Status.JobName = job.Name
	rolloutTest.Status.Phase = rolloutv1alpha1.RolloutTestPhasePending
	rolloutTest.Status.RetryCount = 0
	rolloutTest.Status.ActivePods = 0
	rolloutTest.Status.SucceededPods = 0
	rolloutTest.Status.FailedPods = 0

	if err := r.Status().Update(ctx, rolloutTest); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// isJobFailed checks if a Job has failed by examining its conditions
func (r *RolloutTestReconciler) isJobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
		if condition.Type == batchv1.JobFailureTarget && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobSucceeded checks if a Job has succeeded by examining its conditions
func (r *RolloutTestReconciler) isJobSucceeded(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *RolloutTestReconciler) updateStatus(ctx context.Context, rolloutTest *rolloutv1alpha1.RolloutTest, job *batchv1.Job, canaryRevision string) (ctrl.Result, error) {
	// Update job-related status fields
	rolloutTest.Status.JobName = job.Name
	rolloutTest.Status.ActivePods = job.Status.Active
	rolloutTest.Status.SucceededPods = job.Status.Succeeded
	rolloutTest.Status.FailedPods = job.Status.Failed

	// Calculate retry count from failed pods (each failed pod typically represents a retry)
	// For more accurate retry count, we could track this separately, but failed pods is a good approximation
	rolloutTest.Status.RetryCount = job.Status.Failed

	// Determine phase based on job status
	var phase rolloutv1alpha1.RolloutTestPhase
	var readyStatus metav1.ConditionStatus
	var reason, message string

	// Check if job has started (has start time)
	hasStarted := job.Status.StartTime != nil

	if r.isJobFailed(job) {
		phase = rolloutv1alpha1.RolloutTestPhaseFailed
		readyStatus = metav1.ConditionFalse
		reason = "JobFailed"
		message = "Test job failed"

		// Extract failure message from job conditions if available
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				if condition.Message != "" {
					message = condition.Message
				}
				if condition.Reason != "" {
					reason = condition.Reason
				}
				break
			}
		}
	} else if r.isJobSucceeded(job) {
		phase = rolloutv1alpha1.RolloutTestPhaseSucceeded
		readyStatus = metav1.ConditionTrue
		reason = "JobSucceeded"
		message = "Test job completed successfully"
	} else if hasStarted {
		// Job has started but not completed - it's running
		phase = rolloutv1alpha1.RolloutTestPhaseRunning
		readyStatus = metav1.ConditionFalse
		reason = "JobInProgress"
		message = "Test job is running"
	} else {
		// Job exists but hasn't started yet - it's pending
		phase = rolloutv1alpha1.RolloutTestPhasePending
		readyStatus = metav1.ConditionFalse
		reason = "JobPending"
		message = "Test job is pending"
	}

	rolloutTest.Status.Phase = phase

	// Update conditions
	meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		ObservedGeneration: rolloutTest.Generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	if phase == rolloutv1alpha1.RolloutTestPhaseFailed {
		meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
			Type:               "Failed",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: rolloutTest.Generation,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
			Type:               "Stalled",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: rolloutTest.Generation,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
			Type:               "Failed",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: rolloutTest.Generation,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
			Type:               "Stalled",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: rolloutTest.Generation,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
	}

	// Ensure ObservedCanaryRevision matches the Job's revision.
	// This handles cases where createJob created the job but failed to update the status.
	if revision, ok := job.Labels["rollout.kuberik.io/canary-revision"]; ok {
		rolloutTest.Status.ObservedCanaryRevision = revision
	}

	if err := r.Status().Update(ctx, rolloutTest); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// isCanaryPaused checks if the canary is in a paused state
func (r *RolloutTestReconciler) isCanaryPaused(rollout *kruiserolloutv1beta1.Rollout) bool {
	if rollout.Status.CanaryStatus == nil {
		return false
	}
	// Check if currentStepState indicates paused state
	// Common values: "StepPaused", "Paused", etc.
	state := rollout.Status.CanaryStatus.CurrentStepState
	return state == "StepPaused" || state == "Paused"
}

// isRolloutStalled checks if the rollout is in a stalled state
// isRolloutStalled checks if the rollout is in a stalled state and returns the reason
func (r *RolloutTestReconciler) isRolloutStalled(rollout *kruiserolloutv1beta1.Rollout) (bool, string) {
	if rollout.Status.Conditions == nil {
		return false, ""
	}
	for _, condition := range rollout.Status.Conditions {
		if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") && condition.Status == corev1.ConditionTrue {
			return true, condition.Reason
		}
	}
	return false, ""
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutTestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.RolloutTest{}).
		Named("rollouttest").
		Owns(&batchv1.Job{}).
		Watches(
			&kruiserolloutv1beta1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutTestsForRollout),
		).
		Complete(r)
}

func (r *RolloutTestReconciler) findRolloutTestsForRollout(ctx context.Context, o client.Object) []reconcile.Request {
	rollout := o.(*kruiserolloutv1beta1.Rollout)

	var rolloutTests rolloutv1alpha1.RolloutTestList
	if err := r.List(ctx, &rolloutTests, client.InNamespace(rollout.Namespace)); err != nil {
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, rt := range rolloutTests.Items {
		if rt.Spec.RolloutName == rollout.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      rt.Name,
					Namespace: rt.Namespace,
				},
			})
		}
	}
	return requests
}
