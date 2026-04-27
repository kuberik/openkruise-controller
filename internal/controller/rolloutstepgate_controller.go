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
	"math"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/openkruise-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	kruiserolloutv1beta1 "github.com/openkruise/kruise-rollout-api/rollouts/v1beta1"
)

const (
	// Annotation keys for step configuration (user-set)
	annotationStepReadyTimeoutPrefix = "rollout.kuberik.io/step-%d-ready-timeout"
	annotationStepBakeTimePrefix     = "rollout.kuberik.io/step-%d-bake-time"

	// Internal annotation keys (controller-managed)
	// Store when the step started (when we first entered this step)
	internalAnnotationStepStartedAtPrefix = "internal.rollout.kuberik.io/step-%d-started-at"
	// Store when the step became ready for approval (step paused AND all tests passed)
	internalAnnotationStepReadyAtPrefix = "internal.rollout.kuberik.io/step-%d-ready-at"
	// Store the last canary revision we processed for this step
	internalAnnotationStepLastRevisionPrefix = "internal.rollout.kuberik.io/step-%d-last-revision"
	// Store the last step index we processed (to detect step changes)
	internalAnnotationLastStepIndex = "internal.rollout.kuberik.io/last-step-index"

	// Large duration to effectively pause indefinitely (max int32, ~68 years in seconds)
	maxPauseDuration = int32(math.MaxInt32)

	// kuberikBakeHealthyConditionType is a non-standard condition set on the Kruise Rollout
	// to surface bake-failure blocking state. Unlike the Stalled condition, this does not
	// trigger the retry machinery — it exists solely for observability when canary progression
	// is blocked by a kuberik Rollout bake failure.
	kuberikBakeHealthyConditionType = kruiserolloutv1beta1.RolloutConditionType("KuberikBakeHealthy")
)

// RolloutStepGateReconciler reconciles Rollout steps with auto-approval logic
type RolloutStepGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *RolloutStepGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rollout kruiserolloutv1beta1.Rollout
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only handle canary rollouts
	if rollout.Status.CanaryStatus == nil {
		return ctrl.Result{}, nil
	}

	currentStepIndex := rollout.Status.CanaryStatus.CurrentStepIndex
	if currentStepIndex <= 0 {
		return ctrl.Result{}, nil
	}

	currentRevision := rollout.Status.CanaryStatus.CanaryRevision
	lastRevisionStr := r.getStepAnnotation(&rollout, currentStepIndex, internalAnnotationStepLastRevisionPrefix)

	// Clear Stalled at TOP if context changed (triggered by external change: new canary/step/revision)
	// Check if existing Stalled condition has a different canary revision in its message
	if rollout.Status.Conditions != nil {
		for _, condition := range rollout.Status.Conditions {
			if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") && condition.Status == corev1.ConditionTrue {
				stalledRevision := r.extractCanaryRevisionFromMessage(condition.Message)
				if stalledRevision != "" && stalledRevision != currentRevision {
					log.Info("Clearing Stalled condition - new canary detected",
						"stalledCanary", stalledRevision,
						"currentCanary", currentRevision)
					if r.clearStalledCondition(&rollout) {
						if err := r.Status().Update(ctx, &rollout); err != nil {
							log.Error(err, "failed to clear Stalled condition")
							return ctrl.Result{RequeueAfter: 5 * time.Second}, err
						}
					}
				}
				break
			}
		}
	}

	// Check kuberik rollout bake status
	bakeFailed, bakeMessage, err := r.getBakeFailureStatus(ctx, &rollout)
	if err != nil {
		log.Error(err, "failed to check kuberik rollout bake status")
		// Non-fatal, continue with normal flow
	}
	log.V(1).Info("Bake status check", "bakeFailed", bakeFailed, "message", bakeMessage)

	// Determine retry cutoff from the matching kuberik Rollout. If a retry was
	// requested more recently than the Stalled condition was set, unwind the
	// stalled state: reset failed RolloutTests for the current step, then clear
	// the Stalled condition. Tests whose Failed condition transitioned before the
	// cutoff are treated as pending by evaluateTests (see below) to prevent
	// immediate re-stall during the reset window.
	kuberikRollout, retryCutoff, retryMode, err := r.getKuberikRetryCutoff(ctx, &rollout)
	if err != nil {
		log.Error(err, "failed to fetch kuberik rollout retry timestamp")
		// Non-fatal, continue
	}
	if retryCutoff != nil && r.stalledBefore(&rollout, retryCutoff) {
		log.Info("Retry detected, resetting failed RolloutTests and clearing Stalled",
			"retryCutoff", retryCutoff.Format(time.RFC3339),
			"mode", retryMode)
		if err := r.resetFailedTestsForStep(ctx, rollout.Namespace, rollout.Name, currentStepIndex, retryCutoff, retryMode); err != nil {
			log.Error(err, "failed to reset failed RolloutTests during retry")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// Clear the rollouttest.kuberik.com/retry-mode annotation before clearing
		// the Stalled condition. Doing it first means a transient failure here
		// leaves the Stalled condition in place, so the next reconcile re-enters
		// this branch and retries the cleanup. resetFailedTestsForStep is
		// idempotent, so re-running it on already-reset tests is safe.
		if kuberikRollout != nil {
			if err := clearKuberikRetryModeAnnotation(ctx, r.Client, kuberikRollout); err != nil {
				log.Error(err, "failed to clear retry-mode annotation on kuberik Rollout")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
		// Refresh the step deadline annotations to retryCutoff. Without this, a
		// stale "step-started-at" (from before the retry) can push the step past
		// its step-ready-timeout window the moment Stalled is cleared, so the
		// next reconcile re-stalls with StepReadyTimeoutExceeded — defeating the
		// retry. Using retryCutoff (not time.Now) gives concurrent reconciles a
		// deterministic anchor.
		if err := r.resetStepDeadlineForRetry(ctx, &rollout, currentStepIndex, retryCutoff); err != nil {
			log.Error(err, "failed to refresh step deadline during retry")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// Refetch so clearStalledCondition sees the latest resource version.
		if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
			return ctrl.Result{}, err
		}
		if r.clearStalledCondition(&rollout) {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to clear Stalled condition during retry")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
		// Refetch to get a clean status view for the remainder of the reconcile.
		if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle revision annotation updates
	if lastRevisionStr != "" && lastRevisionStr != currentRevision {
		log.Info("New rollout version detected",
			"step", currentStepIndex,
			"oldRevision", lastRevisionStr,
			"newRevision", currentRevision)

		// Update the last revision annotation
		if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepLastRevisionPrefix, currentRevision); err != nil {
			log.Error(err, "failed to update last revision annotation")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// Also clear the ready-at annotation since this is a new rollout
		if err := r.cleanupStepAnnotations(ctx, &rollout, currentStepIndex); err != nil {
			log.Error(err, "failed to cleanup step annotations for new revision")
			// Non-fatal, continue
		}

		// Clear any existing Stalled condition unconditionally as we have a new revision
		if r.clearStalledCondition(&rollout) {
			log.Info("Clearing Stalled condition for new revision")
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to clear Stalled condition for new revision")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}

		// Refetch rollout after updates
		if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
			return ctrl.Result{}, err
		}
	} else if lastRevisionStr == "" && currentRevision != "" {
		// First time processing this revision, record it
		if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepLastRevisionPrefix, currentRevision); err != nil {
			log.Error(err, "failed to set last revision annotation")
			// Non-fatal, continue
		}
		// Refetch after update
		if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Update last step index annotation and record step start time (only if changed)
	if rollout.Annotations == nil {
		rollout.Annotations = make(map[string]string)
	}
	lastStepIndexStr := rollout.Annotations[internalAnnotationLastStepIndex]
	if lastStepIndexStr != fmt.Sprintf("%d", currentStepIndex) {
		// Step changed, clean up annotations for all other steps
		if err := r.cleanupStepAnnotationsForOtherSteps(ctx, &rollout, currentStepIndex); err != nil {
			log.Error(err, "failed to cleanup annotations for other steps")
			// Non-fatal, continue
		} else {
			// Refetch rollout after cleanup to ensure we have latest state
			if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Record when this step started
		now := time.Now()
		rollout.Annotations[internalAnnotationLastStepIndex] = fmt.Sprintf("%d", currentStepIndex)
		startedAtKey := fmt.Sprintf(internalAnnotationStepStartedAtPrefix, currentStepIndex)
		rollout.Annotations[startedAtKey] = now.Format(time.RFC3339)
		// Also clear ready-at annotation since this is a new step
		readyAtKey := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, currentStepIndex)
		delete(rollout.Annotations, readyAtKey)
		if err := r.Update(ctx, &rollout); err != nil {
			log.Error(err, "failed to update step index and start time annotations")
			// Non-fatal, continue
		} else {
			// Refetch rollout after update
			if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Check if this step has auto-approval configuration
	readyTimeoutStr := r.getStepAnnotation(&rollout, currentStepIndex, annotationStepReadyTimeoutPrefix)
	var readyTimeout time.Duration
	if readyTimeoutStr != "" {
		var err error
		readyTimeout, err = time.ParseDuration(readyTimeoutStr)
		if err != nil {
			log.Error(err, "invalid step-ready-timeout duration", "step", currentStepIndex, "value", readyTimeoutStr)
			return ctrl.Result{}, nil
		}
	} else {
		// Default to infinite timeout (100 years) when no annotation is set
		readyTimeout = 100 * 365 * 24 * time.Hour
	}

	// Get step-bake-time if configured
	bakeTimeStr := r.getStepAnnotation(&rollout, currentStepIndex, annotationStepBakeTimePrefix)
	var bakeTime time.Duration
	if bakeTimeStr != "" {
		bakeTime, err = time.ParseDuration(bakeTimeStr)
		if err != nil {
			log.Error(err, "invalid step-bake-time duration", "step", currentStepIndex, "value", bakeTimeStr)
			return ctrl.Result{}, nil
		}
	}

	// Check if step is actually paused (in StepPaused state)
	isStepPaused := r.isStepPaused(&rollout, currentStepIndex)

	// Get all RolloutTests for this step
	tests, err := r.getRolloutTestsForStep(ctx, rollout.Namespace, rollout.Name, currentStepIndex)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Filter tests that match the current canary revision
	var relevantTests []rolloutv1alpha1.RolloutTest
	for _, test := range tests {
		if test.Status.ObservedCanaryRevision == currentRevision || currentRevision == "" {
			relevantTests = append(relevantTests, test)
		}
	}

	// Evaluate test status.
	// If no RolloutTests exist for this step and a positive step-bake-time is configured,
	// treat tests as implicitly passed so bake-time can still gate auto-approval.
	// Without bake-time, preserve the existing behavior of waiting for tests.
	allPassed := false
	anyFailedTests := false
	failedTestName := ""
	if len(tests) == 0 {
		allPassed = bakeTime > 0
	} else {
		allPassed, anyFailedTests, failedTestName = r.evaluateTests(relevantTests, retryCutoff)
	}
	anyFailed := anyFailedTests || bakeFailed

	now := time.Now()

	// Get when the step started
	startedAtStr := r.getStepAnnotation(&rollout, currentStepIndex, internalAnnotationStepStartedAtPrefix)
	var startedAt time.Time
	if startedAtStr == "" {
		// Step start time not recorded yet, record it now
		startedAt = now
		if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepStartedAtPrefix, startedAt.Format(time.RFC3339)); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Step start time recorded - returning early for requeue", "step", currentStepIndex, "startedAt", startedAt)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else {
		var err error
		startedAt, err = time.Parse(time.RFC3339, startedAtStr)
		if err != nil {
			log.Error(err, "invalid started-at timestamp, resetting", "step", currentStepIndex)
			startedAt = now
			if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepStartedAtPrefix, startedAt.Format(time.RFC3339)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Calculate deadline from when step started
	deadline := startedAt.Add(readyTimeout)

	// Determine when the step became ready for approval
	readyAtStr := r.getStepAnnotation(&rollout, currentStepIndex, internalAnnotationStepReadyAtPrefix)
	var readyAt time.Time

	stepIsReady := false

	// If already ready (annotation set) OR conditions met within deadline
	// Note: We keep readyAt if it was previously set AND tests are still passing, even if past deadline
	// The cleanup logic below (lines 300-318) will clear readyAt if tests fail
	if isStepPaused && allPassed && !bakeFailed && (deadline.After(now) || readyAtStr != "") {
		// Both conditions are met now or were met previously
		stepIsReady = true
		if readyAtStr == "" {
			// First time both conditions are met, record it
			readyAt = now
			if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepReadyAtPrefix, readyAt.Format(time.RFC3339)); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Step ready for approval", "step", currentStepIndex, "readyAt", readyAt)
			// Refetch after update
			if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// We've been ready before, use stored timestamp
			var err error
			readyAt, err = time.Parse(time.RFC3339, readyAtStr)
			if err != nil {
				log.Error(err, "invalid ready-at timestamp, resetting", "step", currentStepIndex)
				readyAt = now
				if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepReadyAtPrefix, readyAt.Format(time.RFC3339)); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	} else {
		// Not ready yet
		if readyAtStr != "" {
			log.Info("Step no longer ready, clearing readyAt annotation", "step", currentStepIndex)
			readyAtKey := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, currentStepIndex)
			if rollout.Annotations != nil {
				if _, exists := rollout.Annotations[readyAtKey]; exists {
					delete(rollout.Annotations, readyAtKey)
					if err := r.Update(ctx, &rollout); err != nil {
						log.Error(err, "failed to clear readyAt annotation")
					} else {
						// Refetch rollout after cleanup
						if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
							return ctrl.Result{}, err
						}
					}
				}
			}
		}

		if !isStepPaused {
			log.Info("Waiting for step to pause", "step", currentStepIndex)
		} else if bakeFailed {
			log.Info("Blocking canary progression: kuberik rollout bake failed", "step", currentStepIndex)
		} else if !allPassed {
			log.Info("Waiting for all tests to pass", "step", currentStepIndex)
		}
	}

	// Set Stalled at END based on current failure state
	// Note: Clearing happens at TOP when external changes detected
	// KuberikBakeHealthy is updated in each branch alongside Stalled so both
	// conditions land in a single Status().Update() call.
	log.V(1).Info("Stalled condition decision point", "bakeFailed", bakeFailed, "anyFailedTests", anyFailedTests, "stepIsReady", stepIsReady, "deadlineExceeded", now.After(deadline))
	bakeHealthModified := r.syncBakeHealthCondition(&rollout, bakeFailed, bakeMessage)
	if anyFailedTests {
		log.Info("Tests failed for step, setting Stalled condition", "step", currentStepIndex)
		message := fmt.Sprintf("Rollout tests failed for current step (test %s, step %d)", failedTestName, currentStepIndex)
		stalledModified, err := r.setStalledCondition(ctx, &rollout, currentStepIndex, "RolloutTestFailed", message)
		if err != nil {
			log.Error(err, "failed to set Stalled condition (annotation cleanup)")
			return ctrl.Result{}, err
		}
		if stalledModified || bakeHealthModified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to set Stalled condition for failed tests")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	} else if !stepIsReady && !bakeFailed && now.After(deadline) {
		log.Info("Step ready-timeout exceeded, keeping paused", "step", currentStepIndex, "deadline", deadline)
		message := fmt.Sprintf("Step %d step-ready-timeout (%v) exceeded at %v for canary %s. Rollout is paused and requires manual intervention.", currentStepIndex, readyTimeout, deadline.Format(time.RFC3339), currentRevision)
		stalledModified, err := r.setStalledCondition(ctx, &rollout, currentStepIndex, "StepReadyTimeoutExceeded", message)
		if err != nil {
			log.Error(err, "failed to set Stalled condition (annotation cleanup)")
			return ctrl.Result{}, err
		}
		if stalledModified || bakeHealthModified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to set Stalled condition")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	} else {
		// We are not stalled (no test failure, no timeout)
		// Clear any existing Stalled condition to enable recovery
		stalledCleared := r.clearStalledCondition(&rollout)
		if stalledCleared {
			log.Info("Clearing Stalled condition - rollout is healthy")
		}
		if stalledCleared || bakeHealthModified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to update Stalled/BakeHealthy conditions")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	}

	// Determine Result
	if anyFailed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if !stepIsReady {
		// Check for timeout again soon
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Step is ready, wait for bake-time
	// Clear Stalled condition handled above in consolidated logic

	// Check if step-bake-time has elapsed since step became ready
	if bakeTime > 0 {
		timeSinceReady := now.Sub(readyAt)
		if timeSinceReady < bakeTime {
			remaining := bakeTime - timeSinceReady
			log.Info("Step ready, waiting for step-bake-time",
				"step", currentStepIndex,
				"timeSinceReady", timeSinceReady,
				"remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	// All conditions met, unpause the step
	log.Info("Auto-approving step, unpausing rollout", "step", currentStepIndex)
	if err := r.unpauseStep(ctx, &rollout, currentStepIndex); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up internal annotation for this step
	if err := r.cleanupStepAnnotations(ctx, &rollout, currentStepIndex); err != nil {
		log.Error(err, "failed to cleanup step annotations", "step", currentStepIndex)
		// Non-fatal, continue
	}

	return ctrl.Result{}, nil
}

// getStepAnnotation retrieves an annotation value for a specific step
func (r *RolloutStepGateReconciler) getStepAnnotation(rollout *kruiserolloutv1beta1.Rollout, stepIndex int32, prefix string) string {
	key := fmt.Sprintf(prefix, stepIndex)
	if rollout.Annotations == nil {
		return ""
	}
	return rollout.Annotations[key]
}

// setStepAnnotation sets an annotation value for a specific step
func (r *RolloutStepGateReconciler) setStepAnnotation(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32, prefix string, value string) error {
	key := fmt.Sprintf(prefix, stepIndex)
	if rollout.Annotations == nil {
		rollout.Annotations = make(map[string]string)
	}
	rollout.Annotations[key] = value
	return r.Update(ctx, rollout)
}

// isStepPaused checks if the step is in a paused state
func (r *RolloutStepGateReconciler) isStepPaused(rollout *kruiserolloutv1beta1.Rollout, stepIndex int32) bool {
	if rollout.Status.CanaryStatus == nil {
		return false
	}
	// Check if currentStepState indicates paused state
	// Common values: "StepPaused", "Paused", etc.
	state := rollout.Status.CanaryStatus.CurrentStepState
	return state == "StepPaused" || state == "Paused"
}

// cleanupStepAnnotations removes internal annotations for a step
func (r *RolloutStepGateReconciler) cleanupStepAnnotations(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32) error {
	if rollout.Annotations == nil {
		return nil
	}

	updated := false
	readyAtKey := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, stepIndex)
	if _, exists := rollout.Annotations[readyAtKey]; exists {
		delete(rollout.Annotations, readyAtKey)
		updated = true
	}

	startedAtKey := fmt.Sprintf(internalAnnotationStepStartedAtPrefix, stepIndex)
	if _, exists := rollout.Annotations[startedAtKey]; exists {
		delete(rollout.Annotations, startedAtKey)
		updated = true
	}

	if updated {
		return r.Update(ctx, rollout)
	}

	return nil
}

// cleanupStepAnnotationsForOtherSteps removes internal annotations for all steps except the current one
func (r *RolloutStepGateReconciler) cleanupStepAnnotationsForOtherSteps(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, currentStepIndex int32) error {
	if rollout.Annotations == nil {
		return nil
	}

	updated := false
	prefixes := []string{
		internalAnnotationStepStartedAtPrefix,
		internalAnnotationStepReadyAtPrefix,
		internalAnnotationStepLastRevisionPrefix,
	}

	// Iterate through a reasonable range of step indices to find and remove annotations for other steps
	// Most rollouts won't have more than 100 steps, but we'll check up to 1000 to be safe
	for stepIndex := int32(1); stepIndex <= 1000; stepIndex++ {
		if stepIndex == currentStepIndex {
			continue
		}

		for _, prefix := range prefixes {
			key := fmt.Sprintf(prefix, stepIndex)
			if _, exists := rollout.Annotations[key]; exists {
				delete(rollout.Annotations, key)
				updated = true
			}
		}
	}

	if updated {
		return r.Update(ctx, rollout)
	}

	return nil
}

// unpauseStep removes the pause from the step
func (r *RolloutStepGateReconciler) unpauseStep(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32) error {
	if rollout.Spec.Strategy.Canary == nil {
		return nil
	}

	steps := rollout.Spec.Strategy.Canary.Steps
	if int(stepIndex) > len(steps) {
		return nil
	}

	stepIdx := int(stepIndex) - 1 // Convert to 0-based index
	step := &steps[stepIdx]

	// Remove pause by setting duration to 0
	zero := int32(0)
	step.Pause.Duration = &zero

	return r.Update(ctx, rollout)
}

// getRolloutTestsForStep retrieves all RolloutTests for a specific rollout and step
func (r *RolloutStepGateReconciler) getRolloutTestsForStep(ctx context.Context, namespace, rolloutName string, stepIndex int32) ([]rolloutv1alpha1.RolloutTest, error) {
	var tests rolloutv1alpha1.RolloutTestList
	if err := r.List(ctx, &tests, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	var result []rolloutv1alpha1.RolloutTest
	for _, test := range tests.Items {
		if test.Spec.RolloutName == rolloutName && test.Spec.StepIndex == stepIndex {
			result = append(result, test)
		}
	}

	return result, nil
}

// evaluateTests checks the status of all tests.
// Returns: (allPassed, anyFailed, failedTestName)
// When retryCutoff is non-nil, a Failed/Cancelled test whose Failed condition
// transitioned before the cutoff is treated as still in progress. This prevents
// RolloutStepGate from re-stalling on stale pre-retry failures during the brief
// window between the retry request and the eventual RolloutTest reset.
func (r *RolloutStepGateReconciler) evaluateTests(tests []rolloutv1alpha1.RolloutTest, retryCutoff *metav1.Time) (bool, bool, string) {
	if len(tests) == 0 {
		// No tests means we can't approve yet - wait for tests to be created
		return false, false, ""
	}

	allPassed := true
	anyFailed := false
	failedTestName := ""
	anyInProgress := false

	for _, test := range tests {
		// Only check Phase - it's the single source of truth
		// This avoids race conditions with partial condition updates
		switch test.Status.Phase {
		case rolloutv1alpha1.RolloutTestPhaseFailed, rolloutv1alpha1.RolloutTestPhaseCancelled:
			if isStaleFailedTest(&test, retryCutoff) {
				// Failure predates the retry; wait for the eventual reset.
				anyInProgress = true
				allPassed = false
				continue
			}
			// Failed or Cancelled = test failed
			anyFailed = true
			allPassed = false
			if failedTestName == "" {
				failedTestName = test.Name
			}
		case rolloutv1alpha1.RolloutTestPhaseSucceeded, rolloutv1alpha1.RolloutTestPhaseSkipped:
			// Test passed (Skipped counts as passing — user intentionally bypassed it).
		default:
			// Running, Pending, WaitingForStep, or empty = still in progress
			anyInProgress = true
			allPassed = false
		}
	}

	// If any failed, return immediately
	if anyFailed {
		return false, true, failedTestName
	}

	// If all passed and none in progress, all passed
	if allPassed && !anyInProgress {
		return true, false, ""
	}

	// Otherwise, still waiting
	return false, false, ""
}

// extractCanaryRevisionFromMessage extracts the canary revision from the Stalled condition message
// Message format: "Step X step-ready-timeout (...) exceeded at ... for canary REVISION. ..."
func (r *RolloutStepGateReconciler) extractCanaryRevisionFromMessage(message string) string {
	// Look for "for canary " followed by the revision
	prefix := "for canary "
	prefixIdx := -1
	for i := 0; i <= len(message)-len(prefix); i++ {
		if message[i:i+len(prefix)] == prefix {
			prefixIdx = i + len(prefix)
			break
		}
	}
	if prefixIdx == -1 || prefixIdx >= len(message) {
		return ""
	}

	// Extract revision until we hit a period or space
	end := prefixIdx
	for end < len(message) && message[end] != '.' && message[end] != ' ' {
		end++
	}

	if end > prefixIdx {
		return message[prefixIdx:end]
	}
	return ""
}

// clearStalledCondition clears the Stalled condition.
// Returns true if the condition was modified, false otherwise.
func (r *RolloutStepGateReconciler) clearStalledCondition(rollout *kruiserolloutv1beta1.Rollout) bool {
	if rollout.Status.Conditions == nil {
		return false
	}

	// Check if Stalled condition exists and is True
	stalledExists := false
	for _, condition := range rollout.Status.Conditions {
		if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") &&
			condition.Status == corev1.ConditionTrue {
			stalledExists = true
			break
		}
	}

	if !stalledExists {
		return false
	}

	// Update the condition to False
	now := metav1.Now()
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
			rollout.Status.Conditions[i].Status = corev1.ConditionFalse
			rollout.Status.Conditions[i].Reason = "WithinDeadline"
			rollout.Status.Conditions[i].Message = "Step is within step-ready-timeout deadline"
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			break
		}
	}

	return true
}

// syncBakeHealthCondition updates the KuberikBakeHealthy condition in-memory.
// When failed=true, sets Status=False with the provided message.
// When failed=false, sets Status=True (healthy/absent-by-default is not used here
// so monitors always see a definitive value).
// Returns true if the condition was modified and a status update is needed.
func (r *RolloutStepGateReconciler) syncBakeHealthCondition(rollout *kruiserolloutv1beta1.Rollout, failed bool, message string) bool {
	now := metav1.Now()
	status := corev1.ConditionTrue
	reason := "BakeHealthy"
	msg := "Kuberik Rollout bake is healthy"
	if failed {
		status = corev1.ConditionFalse
		reason = "BakeFailed"
		msg = message
	}
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kuberikBakeHealthyConditionType {
			if rollout.Status.Conditions[i].Status == status &&
				rollout.Status.Conditions[i].Message == msg {
				return false
			}
			rollout.Status.Conditions[i].Status = status
			rollout.Status.Conditions[i].Reason = reason
			rollout.Status.Conditions[i].Message = msg
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			return true
		}
	}
	rollout.Status.Conditions = append(rollout.Status.Conditions, kruiserolloutv1beta1.RolloutCondition{
		Type:               kuberikBakeHealthyConditionType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		LastUpdateTime:     now,
	})
	return true
}

// getKuberikRetryCutoff returns the linked kuberik Rollout, its LastRetryTimestamp,
// and the retry mode (read from the rollouttest.kuberik.com/retry-mode annotation).
// cutoff is nil when no retry has been recorded. mode defaults to "retry" when the
// annotation is absent or unrecognized. The kuberik Rollout pointer is returned so
// the caller can clear the mode annotation after acting on a fresh retry.
func (r *RolloutStepGateReconciler) getKuberikRetryCutoff(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout) (kuberikRollout *kuberikrolloutv1alpha1.Rollout, cutoff *metav1.Time, mode string, err error) {
	return lookupKuberikRetryContext(ctx, r.Client, rollout, r.findKuberikRolloutForKustomization)
}

// findKuberikRollout resolves the kuberik Rollout associated with the given
// Kruise Rollout via its Kustomization. Returns (nil, nil) when the linkage is
// missing — this is a normal, non-error outcome.
func (r *RolloutStepGateReconciler) findKuberikRollout(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout) (*kuberikrolloutv1alpha1.Rollout, error) {
	return findKuberikRollout(ctx, r.Client, rollout, r.findKuberikRolloutForKustomization)
}

// kuberikResolverFn is the Kustomization → kuberik Rollout resolver. The
// RolloutStepGate reconciler already has this logic (iterates annotations,
// falls back to OCIRepository references); we accept it as a parameter so the
// RolloutTest reconciler can reuse the same resolution by sharing the Kuberik
// Rollout lookup via this package-level helper.
type kuberikResolverFn func(ctx context.Context, kustomization *kustomizev1.Kustomization) (*kuberikrolloutv1alpha1.Rollout, error)

// findKuberikRollout is the package-level version of the method above, accepting
// the Client and resolver as parameters. Used from multiple reconcilers.
func findKuberikRollout(ctx context.Context, c client.Client, rollout *kruiserolloutv1beta1.Rollout, resolve kuberikResolverFn) (*kuberikrolloutv1alpha1.Rollout, error) {
	kustomizeName := rollout.Annotations["kustomize.toolkit.fluxcd.io/name"]
	if kustomizeName == "" {
		kustomizeName = rollout.Labels["kustomize.toolkit.fluxcd.io/name"]
	}
	kustomizeNamespace := rollout.Annotations["kustomize.toolkit.fluxcd.io/namespace"]
	if kustomizeNamespace == "" {
		kustomizeNamespace = rollout.Labels["kustomize.toolkit.fluxcd.io/namespace"]
	}
	if kustomizeName == "" || kustomizeNamespace == "" {
		return nil, nil
	}
	var kustomization kustomizev1.Kustomization
	if err := c.Get(ctx, types.NamespacedName{Name: kustomizeName, Namespace: kustomizeNamespace}, &kustomization); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return resolve(ctx, &kustomization)
}

// lookupKuberikRetryContext reads the linked kuberik Rollout's latest history
// entry for LastRetryTimestamp, and reads the retry mode from the
// rollouttest.kuberik.com/retry-mode annotation on the kuberik Rollout. Returned
// cutoff is nil when no retry has been recorded; mode defaults to RetryModeRetry
// when the annotation is absent or unrecognized.
//
// The kuberik Rollout pointer is returned so callers acting on a fresh retry can
// clear the mode annotation via clearKuberikRetryModeAnnotation.
func lookupKuberikRetryContext(ctx context.Context, c client.Client, rollout *kruiserolloutv1beta1.Rollout, resolve kuberikResolverFn) (*kuberikrolloutv1alpha1.Rollout, *metav1.Time, string, error) {
	kuberikRollout, err := findKuberikRollout(ctx, c, rollout, resolve)
	if err != nil || kuberikRollout == nil {
		return nil, nil, "", err
	}
	var cutoff *metav1.Time
	if len(kuberikRollout.Status.History) > 0 {
		cutoff = kuberikRollout.Status.History[0].LastRetryTimestamp
	}
	return kuberikRollout, cutoff, retryModeFromAnnotation(kuberikRollout), nil
}

// retryModeFromAnnotation reads the retry-mode annotation on the kuberik Rollout
// and normalizes it. Anything other than "skip" defaults to "retry".
func retryModeFromAnnotation(rollout *kuberikrolloutv1alpha1.Rollout) string {
	if v, ok := rollout.Annotations[rolloutv1alpha1.RetryModeAnnotation]; ok && v == rolloutv1alpha1.RetryModeSkip {
		return rolloutv1alpha1.RetryModeSkip
	}
	return rolloutv1alpha1.RetryModeRetry
}

// clearKuberikRetryModeAnnotation removes rollouttest.kuberik.com/retry-mode from the
// kuberik Rollout via JSON patch. The patch is targeted (single op, single path) so
// concurrent updates to other fields (e.g. LastRetryTimestamp by the rollout controller)
// are not at risk of being overwritten the way a MergeFrom diff could be.
//
// Idempotent against the in-memory copy: returns nil when the annotation is absent.
func clearKuberikRetryModeAnnotation(ctx context.Context, c client.Client, rollout *kuberikrolloutv1alpha1.Rollout) error {
	if _, has := rollout.Annotations[rolloutv1alpha1.RetryModeAnnotation]; !has {
		return nil
	}
	// JSON Pointer escape: ~ → ~0, then / → ~1 (order matters to avoid double-escape).
	escaped := strings.ReplaceAll(rolloutv1alpha1.RetryModeAnnotation, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	patch := []byte(fmt.Sprintf(`[{"op":"remove","path":"/metadata/annotations/%s"}]`, escaped))
	return c.Patch(ctx, rollout, client.RawPatch(types.JSONPatchType, patch))
}

// stalledBefore reports whether the Kruise Rollout currently has a Stalled=True
// condition whose LastTransitionTime is strictly before the given cutoff. This
// is the signal that a retry postdates the stall and we should unwind it.
func (r *RolloutStepGateReconciler) stalledBefore(rollout *kruiserolloutv1beta1.Rollout, cutoff *metav1.Time) bool {
	return stalledBefore(rollout, cutoff)
}

// stalledBefore is the same check as the method above, exposed at package level so
// the RolloutTest reconciler can apply the same stale-Stalled guard without
// duplicating the condition-scanning logic.
func stalledBefore(rollout *kruiserolloutv1beta1.Rollout, cutoff *metav1.Time) bool {
	if cutoff == nil {
		return false
	}
	for _, c := range rollout.Status.Conditions {
		if c.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time.Before(cutoff.Time)
		}
	}
	return false
}

// resetStepDeadlineForRetry writes the step-started-at annotation to retryCutoff
// and clears step-ready-at so the step-ready-timeout window restarts after a
// retry. Called only in the retry-detected branch; no-op when annotations are
// already aligned.
func (r *RolloutStepGateReconciler) resetStepDeadlineForRetry(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32, retryCutoff *metav1.Time) error {
	if retryCutoff == nil {
		return nil
	}
	if rollout.Annotations == nil {
		rollout.Annotations = make(map[string]string)
	}
	startedAtKey := fmt.Sprintf(internalAnnotationStepStartedAtPrefix, stepIndex)
	readyAtKey := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, stepIndex)
	desired := retryCutoff.Format(time.RFC3339)
	_, hasReadyAt := rollout.Annotations[readyAtKey]
	if rollout.Annotations[startedAtKey] == desired && !hasReadyAt {
		return nil
	}
	rollout.Annotations[startedAtKey] = desired
	delete(rollout.Annotations, readyAtKey)
	return r.Update(ctx, rollout)
}

// resetFailedTestsForStep handles stale-failed RolloutTests for the given step.
// Behavior depends on retryMode:
//   "skip": mark the test as Skipped (treated as passing).
//   otherwise (retry): reset phase to WaitingForStep so a fresh job is created on
//           next reconcile.
//
// Both modes delete the owned Job: keeping a stale Job around allows
// RolloutTestReconciler.updateStatus to later overwrite our Phase patch with a
// phase derived from the Job's state (e.g. Skipped → Failed when the old job's
// retries finally exhaust). The previous-attempt's outcome is already captured
// on Kubernetes Events and the Kuberik Rollout history — we don't need the Job
// object itself as an audit trail.
//
// Tests that are Succeeded, or failed after the cutoff (fresh failures), are
// left untouched regardless of mode.
func (r *RolloutStepGateReconciler) resetFailedTestsForStep(ctx context.Context, namespace, rolloutName string, stepIndex int32, retryCutoff *metav1.Time, retryMode string) error {
	log := logf.FromContext(ctx)
	tests, err := r.getRolloutTestsForStep(ctx, namespace, rolloutName, stepIndex)
	if err != nil {
		return err
	}
	skipMode := retryMode == rolloutv1alpha1.RetryModeSkip
	for i := range tests {
		test := &tests[i]
		if !isStaleFailedTest(test, retryCutoff) {
			continue
		}

		// Delete the owned Job in both modes. In retry mode a fresh Job needs
		// to be created; in skip mode the old Job's status would otherwise be
		// re-synced onto the RolloutTest by updateStatus, clobbering Skipped.
		var jobs batchv1.JobList
		if err := r.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"rollout-test": test.Name}); err != nil {
			return err
		}
		for j := range jobs.Items {
			if err := r.Delete(ctx, &jobs.Items[j], client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}

		if skipMode {
			test.Status.Phase = rolloutv1alpha1.RolloutTestPhaseSkipped
			test.Status.JobName = ""
			now := metav1.Now()
			msg := "Test skipped via retry annotation (mode=skip)"
			meta.SetStatusCondition(&test.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutTestConditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: test.Generation,
				Reason:             "TestSkipped",
				Message:            msg,
				LastTransitionTime: now,
			})
			meta.SetStatusCondition(&test.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutTestConditionFailed,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: test.Generation,
				Reason:             "TestSkipped",
				Message:            msg,
				LastTransitionTime: now,
			})
			meta.SetStatusCondition(&test.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutTestConditionStalled,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: test.Generation,
				Reason:             "TestSkipped",
				Message:            msg,
				LastTransitionTime: now,
			})
			if err := r.Status().Update(ctx, test); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			log.Info("Marked stale RolloutTest as Skipped", "test", test.Name, "step", stepIndex)
			continue
		}

		test.Status.Phase = rolloutv1alpha1.RolloutTestPhaseWaitingForStep
		test.Status.JobName = ""
		test.Status.RetryCount = 0
		test.Status.ActivePods = 0
		test.Status.SucceededPods = 0
		test.Status.FailedPods = 0
		test.Status.ObservedCanaryRevision = ""
		test.Status.Conditions = nil
		if err := r.Status().Update(ctx, test); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("Reset stale RolloutTest for retry", "test", test.Name, "step", stepIndex)
	}
	return nil
}

// isStaleFailedTest returns true when the test is in Failed/Cancelled phase and
// its Failed condition (or Ready condition as a fallback) transitioned strictly
// before retryCutoff. When retryCutoff is nil, no failure is considered stale.
func isStaleFailedTest(test *rolloutv1alpha1.RolloutTest, retryCutoff *metav1.Time) bool {
	if retryCutoff == nil {
		return false
	}
	if test.Status.Phase != rolloutv1alpha1.RolloutTestPhaseFailed &&
		test.Status.Phase != rolloutv1alpha1.RolloutTestPhaseCancelled {
		return false
	}
	cond := meta.FindStatusCondition(test.Status.Conditions, rolloutv1alpha1.RolloutTestConditionFailed)
	if cond == nil {
		cond = meta.FindStatusCondition(test.Status.Conditions, rolloutv1alpha1.RolloutTestConditionReady)
	}
	if cond == nil {
		// No condition to anchor transition time; be conservative and treat
		// as fresh so we don't silently drop a real failure.
		return false
	}
	return cond.LastTransitionTime.Time.Before(retryCutoff.Time)
}

// getBakeFailureStatus checks if the rollout was deployed by kustomize and if the
// associated kuberik Rollout has a failed bake status in its latest history entry.
// Returns (failed, message, error)
func (r *RolloutStepGateReconciler) getBakeFailureStatus(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout) (bool, string, error) {
	// Check if rollout has kustomize labels or annotations
	kustomizeName := rollout.Annotations["kustomize.toolkit.fluxcd.io/name"]
	if kustomizeName == "" {
		kustomizeName = rollout.Labels["kustomize.toolkit.fluxcd.io/name"]
	}
	kustomizeNamespace := rollout.Annotations["kustomize.toolkit.fluxcd.io/namespace"]
	if kustomizeNamespace == "" {
		kustomizeNamespace = rollout.Labels["kustomize.toolkit.fluxcd.io/namespace"]
	}

	if kustomizeName == "" || kustomizeNamespace == "" {
		// Not deployed by kustomize, skip
		return false, "", nil
	}

	// Get the Kustomization resource
	var kustomization kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kustomizeName,
		Namespace: kustomizeNamespace,
	}, &kustomization); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Kustomization not found, skip
			return false, "", nil
		}
		return false, "", fmt.Errorf("failed to get Kustomization %s/%s: %w", kustomizeNamespace, kustomizeName, err)
	}

	// Find the kuberik Rollout that references this Kustomization
	kuberikRollout, err := r.findKuberikRolloutForKustomization(ctx, &kustomization)
	if err != nil {
		return false, "", fmt.Errorf("failed to find kuberik Rollout for Kustomization: %w", err)
	}
	if kuberikRollout == nil {
		// No kuberik Rollout found, skip
		return false, "", nil
	}

	// Check if the latest history entry has failed bake status
	if len(kuberikRollout.Status.History) == 0 {
		return false, "", nil
	}

	// Check if we can match specific version
	currentVersion := rollout.Annotations["version"]
	var relevantEntry *kuberikrolloutv1alpha1.DeploymentHistoryEntry

	if currentVersion != "" {
		for i := range kuberikRollout.Status.History {
			entry := &kuberikRollout.Status.History[i]
			if entry.Version.Tag == currentVersion {
				relevantEntry = entry
				break
			}
		}
		// If version is specified but not found in history, assume it's not deployed yet
		// Do NOT check History[0] in this case as it might be an old failed version
		if relevantEntry == nil {
			return false, "", nil
		}
	} else if len(kuberikRollout.Status.History) > 0 {
		// Fallback to latest entry if no version specified
		relevantEntry = &kuberikRollout.Status.History[0]
	}

	if relevantEntry != nil && relevantEntry.BakeStatus != nil && *relevantEntry.BakeStatus == kuberikrolloutv1alpha1.BakeStatusFailed {
		// Entry has failed bake status
		log := logf.FromContext(ctx)
		log.Info("Kuberik Rollout has failed bake status",
			"kuberikRollout", kuberikRollout.Name,
			"kuberikRolloutNamespace", kuberikRollout.Namespace,
			"bakeStatus", *relevantEntry.BakeStatus,
			"version", relevantEntry.Version.Tag)

		// Construct message
		currentRevision := rollout.Status.CanaryStatus.CanaryRevision
		message := fmt.Sprintf("Kuberik Rollout %s/%s has failed bake status in deployment of version %s. Rollout is paused and requires manual intervention.", kuberikRollout.Namespace, kuberikRollout.Name, relevantEntry.Version.Tag)
		if currentRevision != "" {
			message = fmt.Sprintf("Kuberik Rollout %s/%s has failed bake status in latest deployment for canary %s (version %s). Rollout is paused and requires manual intervention.", kuberikRollout.Namespace, kuberikRollout.Name, currentRevision, relevantEntry.Version.Tag)
		}
		return true, message, nil
	}

	return false, "", nil
}

// findKuberikRolloutForKustomization finds the kuberik Rollout that references the given Kustomization.
func (r *RolloutStepGateReconciler) findKuberikRolloutForKustomization(ctx context.Context, kustomization *kustomizev1.Kustomization) (*kuberikrolloutv1alpha1.Rollout, error) {
	return findKuberikRolloutForKustomization(ctx, r.Client, kustomization)
}

// findKuberikRolloutForKustomization resolves the kuberik Rollout associated with
// the given Kustomization. It checks, in order:
//  1. Kustomization annotations for rollout references (rollout.kuberik.com/substitute.*.from).
//  2. The OCIRepository that the Kustomization references (rollout.kuberik.com/name or other
//     rollout.kuberik.com/* annotations).
// Returns (nil, nil) when the linkage is missing — a normal, non-error outcome.
func findKuberikRolloutForKustomization(ctx context.Context, c client.Client, kustomization *kustomizev1.Kustomization) (*kuberikrolloutv1alpha1.Rollout, error) {
	log := logf.FromContext(ctx)

	if kustomization.Annotations != nil {
		for key, value := range kustomization.Annotations {
			if strings.HasPrefix(key, "rollout.kuberik.com/substitute.") && strings.HasSuffix(key, ".from") {
				rolloutName := value
				var rollout kuberikrolloutv1alpha1.Rollout
				err := c.Get(ctx, types.NamespacedName{
					Name:      rolloutName,
					Namespace: kustomization.Namespace,
				}, &rollout)
				if err == nil {
					log.V(5).Info("Found kuberik Rollout via Kustomization annotation",
						"rollout", rolloutName,
						"namespace", kustomization.Namespace)
					return &rollout, nil
				}
				log.V(5).Info("Failed to get kuberik Rollout via Kustomization annotation, will try OCIRepository",
					"rollout", rolloutName,
					"namespace", kustomization.Namespace,
					"error", err)
			}
		}
	}

	if kustomization.Spec.SourceRef.Kind == "OCIRepository" {
		ociRepoName := kustomization.Spec.SourceRef.Name
		ociRepoNamespace := kustomization.Namespace
		if kustomization.Spec.SourceRef.Namespace != "" {
			ociRepoNamespace = kustomization.Spec.SourceRef.Namespace
		}

		var ociRepo sourcev1.OCIRepository
		if err := c.Get(ctx, types.NamespacedName{
			Name:      ociRepoName,
			Namespace: ociRepoNamespace,
		}, &ociRepo); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to get OCIRepository %s/%s: %w", ociRepoNamespace, ociRepoName, err)
		}

		if ociRepo.Annotations != nil {
			if rolloutName, exists := ociRepo.Annotations["rollout.kuberik.com/name"]; exists {
				rolloutNamespace := ociRepoNamespace
				if ns, exists := ociRepo.Annotations["rollout.kuberik.com/namespace"]; exists {
					rolloutNamespace = ns
				}
				var rollout kuberikrolloutv1alpha1.Rollout
				if err := c.Get(ctx, types.NamespacedName{
					Name:      rolloutName,
					Namespace: rolloutNamespace,
				}, &rollout); err == nil {
					log.V(5).Info("Found kuberik Rollout via OCIRepository annotation",
						"rollout", rolloutName,
						"namespace", rolloutNamespace)
					return &rollout, nil
				}
			}
			for key, value := range ociRepo.Annotations {
				if strings.HasPrefix(key, "rollout.kuberik.com/") && key != "rollout.kuberik.com/namespace" {
					rolloutName := value
					var rollout kuberikrolloutv1alpha1.Rollout
					if err := c.Get(ctx, types.NamespacedName{
						Name:      rolloutName,
						Namespace: ociRepoNamespace,
					}, &rollout); err == nil {
						log.V(5).Info("Found kuberik Rollout via OCIRepository annotation",
							"rollout", rolloutName,
							"namespace", ociRepoNamespace,
							"annotation", key)
						return &rollout, nil
					}
				}
			}
		}
	}

	return nil, nil
}

// setStalledCondition sets the Stalled condition to True with the given reason and message.
// Returns true if the condition was modified, false otherwise.
// setStalledCondition sets the Stalled condition on the Rollout and clears ready-at annotation
// Returns true if the condition was changed or added
func (r *RolloutStepGateReconciler) setStalledCondition(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32, reason, message string) (bool, error) {
	// First, explicitly clear the ready-at annotation since the rollout is stalled
	readyAtKey := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, stepIndex)
	if _, exists := rollout.Annotations[readyAtKey]; exists {
		delete(rollout.Annotations, readyAtKey)
		// We must update the object metadata on the server
		if err := r.Update(ctx, rollout); err != nil {
			return false, err
		}
		// Note: r.Update refreshes the rollout object, including Status.
		// So we are working with the latest Status from server, which is what we want.
	}

	// Initialize conditions slice if nil
	if rollout.Status.Conditions == nil {
		rollout.Status.Conditions = []kruiserolloutv1beta1.RolloutCondition{}
	}

	now := metav1.Now()
	modified := false

	// Find and update existing condition or add new one
	found := false
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
			if rollout.Status.Conditions[i].Status == corev1.ConditionTrue &&
				rollout.Status.Conditions[i].Reason == reason &&
				rollout.Status.Conditions[i].Message == message {
				// Condition already set with same status, reason, and message, no need to update
				return false, nil
			}
			rollout.Status.Conditions[i].Status = corev1.ConditionTrue
			rollout.Status.Conditions[i].Reason = reason
			rollout.Status.Conditions[i].Message = message
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			found = true
			modified = true
			break
		}
	}

	if !found {
		rollout.Status.Conditions = append(rollout.Status.Conditions, kruiserolloutv1beta1.RolloutCondition{
			Type:               kruiserolloutv1beta1.RolloutConditionType("Stalled"),
			Status:             corev1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
			LastUpdateTime:     now,
		})
		modified = true
	}

	return modified, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutStepGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kruiserolloutv1beta1.Rollout{}).
		Named("rolloutstepgate").
		Watches(
			&rolloutv1alpha1.RolloutTest{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutsForRolloutTest),
		).
		Watches(
			&kuberikrolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findKruiseRolloutsForKuberikRollout),
		).
		Complete(r)
}

// findRolloutsForRolloutTest finds Rollouts that should be reconciled when a RolloutTest changes
func (r *RolloutStepGateReconciler) findRolloutsForRolloutTest(ctx context.Context, o client.Object) []reconcile.Request {
	test := o.(*rolloutv1alpha1.RolloutTest)

	var rollout kruiserolloutv1beta1.Rollout
	if err := r.Get(ctx, types.NamespacedName{
		Name:      test.Spec.RolloutName,
		Namespace: test.Namespace,
	}, &rollout); err != nil {
		return []reconcile.Request{}
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      rollout.Name,
				Namespace: rollout.Namespace,
			},
		},
	}
}

// findKruiseRolloutsForKuberikRollout finds Kruise Rollouts that should be reconciled when a Kuberik Rollout changes
func (r *RolloutStepGateReconciler) findKruiseRolloutsForKuberikRollout(ctx context.Context, o client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)
	kuberikRollout := o.(*kuberikrolloutv1alpha1.Rollout)

	// List all Kruise Rollouts that might reference this Kuberik Rollout
	var kruiseRollouts kruiserolloutv1beta1.RolloutList
	if err := r.List(ctx, &kruiseRollouts); err != nil {
		log.Error(err, "failed to list Kruise Rollouts")
		return []reconcile.Request{}
	}

	var requests []reconcile.Request

	// Check each Kruise Rollout to see if it references this Kuberik Rollout
	for _, kruiseRollout := range kruiseRollouts.Items {
		// Skip if not deployed by kustomize
		kustomizeName := kruiseRollout.Annotations["kustomize.toolkit.fluxcd.io/name"]
		if kustomizeName == "" {
			kustomizeName = kruiseRollout.Labels["kustomize.toolkit.fluxcd.io/name"]
		}
		kustomizeNamespace := kruiseRollout.Annotations["kustomize.toolkit.fluxcd.io/namespace"]
		if kustomizeNamespace == "" {
			kustomizeNamespace = kruiseRollout.Labels["kustomize.toolkit.fluxcd.io/namespace"]
		}

		if kustomizeName == "" || kustomizeNamespace == "" {
			continue
		}

		// Get the Kustomization
		var kustomization kustomizev1.Kustomization
		if err := r.Get(ctx, types.NamespacedName{
			Name:      kustomizeName,
			Namespace: kustomizeNamespace,
		}, &kustomization); err != nil {
			continue
		}

		// Use existing logic to find the Kuberik Rollout for this Kustomization
		foundKuberikRollout, err := r.findKuberikRolloutForKustomization(ctx, &kustomization)
		if err != nil {
			log.V(5).Info("Error finding Kuberik Rollout for Kustomization",
				"kustomization", kustomizeName,
				"namespace", kustomizeNamespace,
				"error", err)
			continue
		}

		// If this Kustomization references the Kuberik Rollout that changed, queue reconciliation
		if foundKuberikRollout != nil &&
			foundKuberikRollout.Name == kuberikRollout.Name &&
			foundKuberikRollout.Namespace == kuberikRollout.Namespace {
			log.V(5).Info("Queuing Kruise Rollout for reconciliation due to Kuberik Rollout change",
				"kruiseRollout", kruiseRollout.Name,
				"kruiseRolloutNamespace", kruiseRollout.Namespace,
				"kuberikRollout", kuberikRollout.Name,
				"kuberikRolloutNamespace", kuberikRollout.Namespace)
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      kruiseRollout.Name,
					Namespace: kruiseRollout.Namespace,
				},
			})
		}
	}

	return requests
}
