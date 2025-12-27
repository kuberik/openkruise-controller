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
	annotationStepMaxWaitPrefix             = "rollout.kuberik.io/step-%d-max-wait"
	annotationStepMinWaitAfterSuccessPrefix = "rollout.kuberik.io/step-%d-min-wait-after-success"

	// Internal annotation keys (controller-managed)
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

	// Check if this is a new rollout version - if so, clear Stalled condition
	// Do this early before any Updates that might affect annotations
	currentRevision := rollout.Status.CanaryStatus.CanaryRevision

	// Check if Stalled condition exists for a different canary revision
	if err := r.clearStalledConditionIfNewCanary(ctx, &rollout, currentRevision); err != nil {
		log.Error(err, "failed to check/clear Stalled condition for new canary")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Check if rollout was deployed by kustomize and if kuberik rollout has failed bake status
	if err := r.checkKuberikRolloutBakeStatus(ctx, &rollout); err != nil {
		log.Error(err, "failed to check kuberik rollout bake status")
		// Non-fatal, continue with normal flow
	}

	// Refetch rollout after checking kuberik bake status to ensure we have latest status
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	lastRevisionStr := r.getStepAnnotation(&rollout, currentStepIndex, internalAnnotationStepLastRevisionPrefix)
	if lastRevisionStr != "" && lastRevisionStr != currentRevision {
		// New rollout version detected, clear Stalled condition
		log.Info("New rollout version detected, clearing Stalled condition",
			"step", currentStepIndex,
			"oldRevision", lastRevisionStr,
			"newRevision", currentRevision)
		if err := r.clearStalledCondition(ctx, &rollout); err != nil {
			log.Error(err, "failed to clear Stalled condition for new revision")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
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

	// Check if step has changed - if so, clear Stalled condition
	if rollout.Annotations != nil {
		lastStepIndexStr := rollout.Annotations[internalAnnotationLastStepIndex]
		if lastStepIndexStr != "" {
			var lastStepIndex int32
			if _, err := fmt.Sscanf(lastStepIndexStr, "%d", &lastStepIndex); err == nil {
				if lastStepIndex != currentStepIndex {
					// Step changed, clear Stalled condition
					log.Info("Step changed, clearing Stalled condition",
						"oldStep", lastStepIndex,
						"newStep", currentStepIndex)
					if err := r.clearStalledCondition(ctx, &rollout); err != nil {
						log.Error(err, "failed to clear Stalled condition for step change")
						return ctrl.Result{RequeueAfter: 5 * time.Second}, err
					}
					// Refetch rollout after status update
					if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
						return ctrl.Result{}, err
					}
				}
			}
		}
	}

	// Update last step index annotation (only if changed)
	if rollout.Annotations == nil {
		rollout.Annotations = make(map[string]string)
	}
	lastStepIndexStr := rollout.Annotations[internalAnnotationLastStepIndex]
	if lastStepIndexStr != fmt.Sprintf("%d", currentStepIndex) {
		rollout.Annotations[internalAnnotationLastStepIndex] = fmt.Sprintf("%d", currentStepIndex)
		if err := r.Update(ctx, &rollout); err != nil {
			log.Error(err, "failed to update last step index annotation")
			// Non-fatal, continue
		} else {
			// Refetch rollout after update
			if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Check if this step has auto-approval configuration
	maxWaitStr := r.getStepAnnotation(&rollout, currentStepIndex, annotationStepMaxWaitPrefix)
	if maxWaitStr == "" {
		// No auto-approval config for this step, nothing to do
		return ctrl.Result{}, nil
	}

	maxWait, err := time.ParseDuration(maxWaitStr)
	if err != nil {
		log.Error(err, "invalid max-wait duration", "step", currentStepIndex, "value", maxWaitStr)
		return ctrl.Result{}, nil
	}

	// Get min-wait-after-success if configured
	minWaitAfterSuccessStr := r.getStepAnnotation(&rollout, currentStepIndex, annotationStepMinWaitAfterSuccessPrefix)
	var minWaitAfterSuccess time.Duration
	if minWaitAfterSuccessStr != "" {
		minWaitAfterSuccess, err = time.ParseDuration(minWaitAfterSuccessStr)
		if err != nil {
			log.Error(err, "invalid min-wait-after-success duration", "step", currentStepIndex, "value", minWaitAfterSuccessStr)
			return ctrl.Result{}, nil
		}
	}

	// Ensure the step is paused
	if err := r.ensureStepPaused(ctx, &rollout, currentStepIndex); err != nil {
		return ctrl.Result{}, err
	}

	// Refetch rollout after ensureStepPaused (which may have updated the rollout)
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		return ctrl.Result{}, err
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
	allPassed, anyFailed := r.evaluateTests(relevantTests)

	now := time.Now()

	// Check for failure conditions first
	if anyFailed {
		log.Info("Tests failed for step, keeping paused", "step", currentStepIndex)
		// Keep paused - user needs to intervene
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Determine when the step became ready for approval
	// Ready = step paused AND all tests passed (whichever happens later)
	readyAtStr := r.getStepAnnotation(&rollout, currentStepIndex, internalAnnotationStepReadyAtPrefix)
	var readyAt time.Time

	if isStepPaused && allPassed {
		// Both conditions are met now
		if readyAtStr == "" {
			// First time both conditions are met, record it
			readyAt = now
			if err := r.setStepAnnotation(ctx, &rollout, currentStepIndex, internalAnnotationStepReadyAtPrefix, readyAt.Format(time.RFC3339)); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Step ready for approval", "step", currentStepIndex, "readyAt", readyAt)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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
		// Not ready yet - either step not paused or tests not all passed
		// Don't start the approval timer yet
		if !isStepPaused {
			log.Info("Waiting for step to pause", "step", currentStepIndex)
		} else if !allPassed {
			log.Info("Waiting for all tests to pass", "step", currentStepIndex)
		}
		// Requeue to check again
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Calculate deadline from when step became ready
	deadline := readyAt.Add(maxWait)

	// Check for timeout
	if now.After(deadline) {
		log.Info("Step max-wait exceeded, keeping paused", "step", currentStepIndex, "deadline", deadline, "now", now)
		// Refetch rollout to ensure we have latest resource version before status update
		if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
			return ctrl.Result{}, err
		}
		// Set Stalled condition for kstatus compatibility (maps to FailedStatus)
		if err := r.setStalledCondition(ctx, &rollout, currentStepIndex, deadline, maxWait); err != nil {
			log.Error(err, "failed to set Stalled condition")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// Keep paused - user needs to intervene
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Clear Stalled condition if it exists (we're within deadline)
	if err := r.clearStalledCondition(ctx, &rollout); err != nil {
		log.Error(err, "failed to clear Stalled condition")
		// Non-fatal, continue
	}

	// Check if min-wait-after-success has elapsed since step became ready
	if minWaitAfterSuccess > 0 {
		timeSinceReady := now.Sub(readyAt)
		if timeSinceReady < minWaitAfterSuccess {
			remaining := minWaitAfterSuccess - timeSinceReady
			log.Info("Step ready, waiting for min-wait-after-success",
				"step", currentStepIndex,
				"timeSinceReady", timeSinceReady,
				"remaining", remaining)
			// Requeue more frequently as we approach the deadline
			requeueAfter := remaining
			if time.Until(deadline) < remaining {
				requeueAfter = time.Until(deadline)
				if requeueAfter < 2*time.Second {
					requeueAfter = 2 * time.Second
				}
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
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

	key := fmt.Sprintf(internalAnnotationStepReadyAtPrefix, stepIndex)
	if _, exists := rollout.Annotations[key]; exists {
		delete(rollout.Annotations, key)
		return r.Update(ctx, rollout)
	}

	return nil
}

// ensureStepPaused ensures the rollout step is paused
func (r *RolloutStepGateReconciler) ensureStepPaused(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32) error {
	if rollout.Spec.Strategy.Canary == nil {
		return nil
	}

	steps := rollout.Spec.Strategy.Canary.Steps
	if int(stepIndex) > len(steps) {
		return nil
	}

	stepIdx := int(stepIndex) - 1 // Convert to 0-based index
	step := &steps[stepIdx]

	// Check if step already has a long pause duration
	if step.Pause.Duration != nil && *step.Pause.Duration >= maxPauseDuration {
		return nil
	}

	// Set pause duration to max value
	duration := maxPauseDuration
	step.Pause.Duration = &duration

	return r.Update(ctx, rollout)
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
// Returns: (allPassed, anyFailed)
func (r *RolloutStepGateReconciler) evaluateTests(tests []rolloutv1alpha1.RolloutTest) (bool, bool) {
	if len(tests) == 0 {
		// No tests means we can't approve yet - wait for tests to be created
		return false, false
	}

	allPassed := true
	anyFailed := false
	anyInProgress := false

	for _, test := range tests {
		ready := false
		failed := false

		for _, condition := range test.Status.Conditions {
			if condition.Type == "Ready" {
				ready = condition.Status == metav1.ConditionTrue
			}
			if condition.Type == "Failed" {
				failed = condition.Status == metav1.ConditionTrue
			}
		}

		if failed {
			anyFailed = true
			allPassed = false
		} else if ready {
			// Test passed
		} else {
			// Test still in progress
			anyInProgress = true
			allPassed = false
		}
	}

	// If any failed, return immediately
	if anyFailed {
		return false, true
	}

	// If all passed and none in progress, all passed
	if allPassed && !anyInProgress {
		return true, false
	}

	// Otherwise, still waiting
	return false, false
}

// setStalledCondition sets the Stalled condition on the Rollout when max wait is exceeded
func (r *RolloutStepGateReconciler) setStalledCondition(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, stepIndex int32, deadline time.Time, maxWait time.Duration) error {
	// Check if condition already exists and is up to date
	if rollout.Status.Conditions != nil {
		for _, condition := range rollout.Status.Conditions {
			if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") && condition.Status == corev1.ConditionTrue {
				// Condition already set, no need to update
				return nil
			}
		}
	}

	// Initialize conditions slice if nil
	if rollout.Status.Conditions == nil {
		rollout.Status.Conditions = []kruiserolloutv1beta1.RolloutCondition{}
	}

	currentRevision := rollout.Status.CanaryStatus.CanaryRevision
	message := fmt.Sprintf("Step %d max-wait (%v) exceeded at %v for canary %s. Rollout is paused and requires manual intervention.", stepIndex, maxWait, deadline.Format(time.RFC3339), currentRevision)
	now := metav1.Now()

	// Find and update existing condition or add new one
	found := false
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
			rollout.Status.Conditions[i].Status = corev1.ConditionTrue
			rollout.Status.Conditions[i].Reason = "MaxWaitExceeded"
			rollout.Status.Conditions[i].Message = message
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			found = true
			break
		}
	}

	if !found {
		rollout.Status.Conditions = append(rollout.Status.Conditions, kruiserolloutv1beta1.RolloutCondition{
			Type:               kruiserolloutv1beta1.RolloutConditionType("Stalled"),
			Status:             corev1.ConditionTrue,
			Reason:             "MaxWaitExceeded",
			Message:            message,
			LastTransitionTime: now,
			LastUpdateTime:     now,
		})
	}

	return r.Status().Update(ctx, rollout)
}

// clearStalledConditionIfNewCanary checks if the Stalled condition is for a different canary revision
// and clears it if so. The canary revision is embedded in the condition message.
func (r *RolloutStepGateReconciler) clearStalledConditionIfNewCanary(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, currentRevision string) error {
	if rollout.Status.Conditions == nil || currentRevision == "" {
		return nil
	}

	// Find Stalled condition and check if it's for a different canary
	for _, condition := range rollout.Status.Conditions {
		if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") && condition.Status == corev1.ConditionTrue {
			// Extract canary revision from message
			// Message format: "Step X max-wait (...) exceeded at ... for canary REVISION. ..."
			stalledRevision := r.extractCanaryRevisionFromMessage(condition.Message)
			if stalledRevision != "" && stalledRevision != currentRevision {
				// Stalled condition is for a different canary, clear it
				log := logf.FromContext(ctx)
				log.Info("Stalled condition is for different canary, clearing it",
					"stalledCanary", stalledRevision,
					"currentCanary", currentRevision)
				return r.clearStalledCondition(ctx, rollout)
			}
			break
		}
	}

	return nil
}

// extractCanaryRevisionFromMessage extracts the canary revision from the Stalled condition message
// Message format: "Step X max-wait (...) exceeded at ... for canary REVISION. ..."
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

// clearStalledCondition clears the Stalled condition on the Rollout
// It does NOT clear conditions with Reason "KuberikRolloutBakeFailed" as those should persist
func (r *RolloutStepGateReconciler) clearStalledCondition(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout) error {
	if rollout.Status.Conditions == nil {
		return nil
	}

	// Check if Stalled condition exists and is True, but not for kuberik bake failure
	stalledExists := false
	for _, condition := range rollout.Status.Conditions {
		if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") &&
			condition.Status == corev1.ConditionTrue &&
			condition.Reason != "KuberikRolloutBakeFailed" {
			stalledExists = true
			break
		}
	}

	if !stalledExists {
		return nil
	}

	// Refetch rollout to ensure we have latest resource version before status update
	// This is important to avoid conflicts
	namespacedName := types.NamespacedName{Name: rollout.Name, Namespace: rollout.Namespace}
	if err := r.Get(ctx, namespacedName, rollout); err != nil {
		return err
	}

	// Update the condition to False (only if not KuberikRolloutBakeFailed)
	now := metav1.Now()
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") &&
			rollout.Status.Conditions[i].Reason != "KuberikRolloutBakeFailed" {
			rollout.Status.Conditions[i].Status = corev1.ConditionFalse
			rollout.Status.Conditions[i].Reason = "WithinDeadline"
			rollout.Status.Conditions[i].Message = "Step is within max-wait deadline"
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			break
		}
	}

	return r.Status().Update(ctx, rollout)
}

// checkKuberikRolloutBakeStatus checks if the rollout was deployed by kustomize and if the
// associated kuberik Rollout has a failed bake status in its latest history entry.
// If so, sets the Stalled condition.
func (r *RolloutStepGateReconciler) checkKuberikRolloutBakeStatus(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout) error {
	log := logf.FromContext(ctx)

	// Check if rollout has kustomize annotations
	kustomizeName := rollout.Annotations["kustomize.toolkit.fluxcd.io/name"]
	kustomizeNamespace := rollout.Annotations["kustomize.toolkit.fluxcd.io/namespace"]
	if kustomizeName == "" || kustomizeNamespace == "" {
		// Not deployed by kustomize, skip
		return nil
	}

	// Get the Kustomization resource
	var kustomization kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kustomizeName,
		Namespace: kustomizeNamespace,
	}, &kustomization); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Kustomization not found, skip
			return nil
		}
		return fmt.Errorf("failed to get Kustomization %s/%s: %w", kustomizeNamespace, kustomizeName, err)
	}

	// Find the kuberik Rollout that references this Kustomization
	// We'll look for Rollouts that have annotations referencing this Kustomization
	// or check the OCIRepository that the Kustomization references
	kuberikRollout, err := r.findKuberikRolloutForKustomization(ctx, &kustomization)
	if err != nil {
		return fmt.Errorf("failed to find kuberik Rollout for Kustomization: %w", err)
	}
	if kuberikRollout == nil {
		// No kuberik Rollout found, skip
		return nil
	}

	// Check if the latest history entry has failed bake status
	if len(kuberikRollout.Status.History) == 0 {
		return nil
	}

	latestEntry := kuberikRollout.Status.History[0]
	if latestEntry.BakeStatus != nil && *latestEntry.BakeStatus == kuberikrolloutv1alpha1.BakeStatusFailed {
		// Latest entry has failed bake status, set Stalled condition
		log.Info("Kuberik Rollout has failed bake status, setting Stalled condition",
			"kuberikRollout", kuberikRollout.Name,
			"kuberikRolloutNamespace", kuberikRollout.Namespace,
			"bakeStatus", *latestEntry.BakeStatus)

		// Set Stalled condition with message indicating kuberik rollout bake failure
		currentRevision := rollout.Status.CanaryStatus.CanaryRevision
		message := fmt.Sprintf("Kuberik Rollout %s/%s has failed bake status in latest deployment. Rollout is paused and requires manual intervention.", kuberikRollout.Namespace, kuberikRollout.Name)
		if currentRevision != "" {
			message = fmt.Sprintf("Kuberik Rollout %s/%s has failed bake status in latest deployment for canary %s. Rollout is paused and requires manual intervention.", kuberikRollout.Namespace, kuberikRollout.Name, currentRevision)
		}
		err := r.setStalledConditionForKuberikBakeFailure(ctx, rollout, message)
		if err != nil {
			return err
		}
		return nil
	}

	// If bake status is not failed, clear any existing Stalled condition with KuberikRolloutBakeFailed reason
	if rollout.Status.Conditions != nil {
		for _, condition := range rollout.Status.Conditions {
			if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") &&
				condition.Status == corev1.ConditionTrue &&
				condition.Reason == "KuberikRolloutBakeFailed" {
				// Clear the condition
				return r.clearStalledCondition(ctx, rollout)
			}
		}
	}

	return nil
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

// setStalledConditionForKuberikBakeFailure sets the Stalled condition when kuberik rollout bake fails
func (r *RolloutStepGateReconciler) setStalledConditionForKuberikBakeFailure(ctx context.Context, rollout *kruiserolloutv1beta1.Rollout, message string) error {
	// Refetch rollout to ensure we have latest resource version before status update
	// This is important to avoid conflicts
	namespacedName := types.NamespacedName{Name: rollout.Name, Namespace: rollout.Namespace}
	if err := r.Get(ctx, namespacedName, rollout); err != nil {
		return err
	}

	// Check if condition already exists and is up to date
	if rollout.Status.Conditions != nil {
		for _, condition := range rollout.Status.Conditions {
			if condition.Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") &&
				condition.Status == corev1.ConditionTrue &&
				condition.Reason == "KuberikRolloutBakeFailed" {
				// Condition already set with same reason, no need to update
				return nil
			}
		}
	}

	// Initialize conditions slice if nil
	if rollout.Status.Conditions == nil {
		rollout.Status.Conditions = []kruiserolloutv1beta1.RolloutCondition{}
	}

	now := metav1.Now()

	// Find and update existing condition or add new one
	found := false
	for i := range rollout.Status.Conditions {
		if rollout.Status.Conditions[i].Type == kruiserolloutv1beta1.RolloutConditionType("Stalled") {
			rollout.Status.Conditions[i].Status = corev1.ConditionTrue
			rollout.Status.Conditions[i].Reason = "KuberikRolloutBakeFailed"
			rollout.Status.Conditions[i].Message = message
			rollout.Status.Conditions[i].LastTransitionTime = now
			rollout.Status.Conditions[i].LastUpdateTime = now
			found = true
			break
		}
	}

	if !found {
		rollout.Status.Conditions = append(rollout.Status.Conditions, kruiserolloutv1beta1.RolloutCondition{
			Type:               kruiserolloutv1beta1.RolloutConditionType("Stalled"),
			Status:             corev1.ConditionTrue,
			Reason:             "KuberikRolloutBakeFailed",
			Message:            message,
			LastTransitionTime: now,
			LastUpdateTime:     now,
		})
	}

	err := r.Status().Update(ctx, rollout)
	if err != nil {
		return err
	}
	return nil
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
