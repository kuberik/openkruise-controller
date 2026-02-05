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

	corev1 "k8s.io/api/core/v1"
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
)

// RolloutStepGateReconciler reconciles Rollout steps with auto-approval logic
type RolloutStepGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
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

	// Evaluate test status
	allPassed, anyFailedTests, failedTestName := r.evaluateTests(relevantTests)
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
	if isStepPaused && allPassed && (deadline.After(now) || readyAtStr != "") {
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
		} else if !allPassed {
			log.Info("Waiting for all tests to pass", "step", currentStepIndex)
		}
	}

	// Set Stalled at END based on current failure state
	// Note: Clearing happens at TOP when external changes detected
	log.V(1).Info("Stalled condition decision point", "bakeFailed", bakeFailed, "anyFailedTests", anyFailedTests, "stepIsReady", stepIsReady, "deadlineExceeded", now.After(deadline))
	if bakeFailed {
		log.Info("Kuberik rollout bake failed, setting Stalled condition", "step", currentStepIndex)
		modified, err := r.setStalledCondition(ctx, &rollout, currentStepIndex, "KuberikRolloutBakeFailed", bakeMessage)
		if err != nil {
			log.Error(err, "failed to set Stalled condition (annotation cleanup)")
			return ctrl.Result{}, err
		}
		if modified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to set Stalled condition")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	} else if anyFailedTests {
		log.Info("Tests failed for step, setting Stalled condition", "step", currentStepIndex)
		message := fmt.Sprintf("Rollout tests failed for current step (test %s, step %d)", failedTestName, currentStepIndex)
		modified, err := r.setStalledCondition(ctx, &rollout, currentStepIndex, "RolloutTestFailed", message)
		if err != nil {
			log.Error(err, "failed to set Stalled condition (annotation cleanup)")
			return ctrl.Result{}, err
		}
		if modified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to set Stalled condition for failed tests")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	} else if !stepIsReady && now.After(deadline) {
		log.Info("Step ready-timeout exceeded, keeping paused", "step", currentStepIndex, "deadline", deadline)
		message := fmt.Sprintf("Step %d step-ready-timeout (%v) exceeded at %v for canary %s. Rollout is paused and requires manual intervention.", currentStepIndex, readyTimeout, deadline.Format(time.RFC3339), currentRevision)
		modified, err := r.setStalledCondition(ctx, &rollout, currentStepIndex, "StepReadyTimeoutExceeded", message)
		if err != nil {
			log.Error(err, "failed to set Stalled condition (annotation cleanup)")
			return ctrl.Result{}, err
		}
		if modified {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to set Stalled condition")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	} else {
		// We are not stalled (no bake failure, no test failure, no timeout)
		// Clear any existing Stalled condition to enable recovery
		if r.clearStalledCondition(&rollout) {
			log.Info("Clearing Stalled condition - rollout is healthy")
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "failed to clear Stalled condition")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	}
	// Note: We don't clear here when healthy - clearing only happens at TOP when context changes

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

// evaluateTests checks the status of all tests
// Returns: (allPassed, anyFailed, failedTestName)
func (r *RolloutStepGateReconciler) evaluateTests(tests []rolloutv1alpha1.RolloutTest) (bool, bool, string) {
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
			// Failed or Cancelled = test failed
			anyFailed = true
			allPassed = false
			if failedTestName == "" {
				failedTestName = test.Name
			}
		case rolloutv1alpha1.RolloutTestPhaseSucceeded:
			// Test passed, continue to next test
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
// It checks:
// 1. Kustomization annotations for rollout references
// 2. OCIRepository that the Kustomization references
// 3. List all Rollouts and check for matching references
func (r *RolloutStepGateReconciler) findKuberikRolloutForKustomization(ctx context.Context, kustomization *kustomizev1.Kustomization) (*kuberikrolloutv1alpha1.Rollout, error) {
	log := logf.FromContext(ctx)

	// First, check if Kustomization has annotations that reference a kuberik Rollout
	// Look for annotations like "rollout.kuberik.com/substitute.*.from"
	if kustomization.Annotations != nil {
		for key, value := range kustomization.Annotations {
			if strings.HasPrefix(key, "rollout.kuberik.com/substitute.") && strings.HasSuffix(key, ".from") {
				// Found a rollout reference, try to get it
				rolloutName := value
				// Try in the same namespace as the Kustomization first
				var rollout kuberikrolloutv1alpha1.Rollout
				err := r.Get(ctx, types.NamespacedName{
					Name:      rolloutName,
					Namespace: kustomization.Namespace,
				}, &rollout)
				if err == nil {
					log.V(5).Info("Found kuberik Rollout via Kustomization annotation",
						"rollout", rolloutName,
						"namespace", kustomization.Namespace)
					return &rollout, nil
				}
				// If Get failed, log but continue to check OCIRepository path
				log.V(5).Info("Failed to get kuberik Rollout via Kustomization annotation, will try OCIRepository",
					"rollout", rolloutName,
					"namespace", kustomization.Namespace,
					"error", err)
			}
		}
	}

	// If Kustomization references an OCIRepository, check if any Rollout references it
	if kustomization.Spec.SourceRef.Kind == "OCIRepository" {
		ociRepoName := kustomization.Spec.SourceRef.Name
		ociRepoNamespace := kustomization.Namespace
		if kustomization.Spec.SourceRef.Namespace != "" {
			ociRepoNamespace = kustomization.Spec.SourceRef.Namespace
		}

		// Get the OCIRepository
		var ociRepo sourcev1.OCIRepository
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ociRepoName,
			Namespace: ociRepoNamespace,
		}, &ociRepo); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to get OCIRepository %s/%s: %w", ociRepoNamespace, ociRepoName, err)
		}

		// Check if OCIRepository has annotations referencing a kuberik Rollout
		if ociRepo.Annotations != nil {
			// Look for direct rollout reference annotation
			if rolloutName, exists := ociRepo.Annotations["rollout.kuberik.com/name"]; exists {
				rolloutNamespace := ociRepoNamespace
				if ns, exists := ociRepo.Annotations["rollout.kuberik.com/namespace"]; exists {
					rolloutNamespace = ns
				}
				var rollout kuberikrolloutv1alpha1.Rollout
				if err := r.Get(ctx, types.NamespacedName{
					Name:      rolloutName,
					Namespace: rolloutNamespace,
				}, &rollout); err == nil {
					log.V(5).Info("Found kuberik Rollout via OCIRepository annotation",
						"rollout", rolloutName,
						"namespace", rolloutNamespace)
					return &rollout, nil
				}
			}
			// Also check for other rollout.kuberik.com annotations
			for key, value := range ociRepo.Annotations {
				if strings.HasPrefix(key, "rollout.kuberik.com/") && key != "rollout.kuberik.com/namespace" {
					rolloutName := value
					var rollout kuberikrolloutv1alpha1.Rollout
					if err := r.Get(ctx, types.NamespacedName{
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
