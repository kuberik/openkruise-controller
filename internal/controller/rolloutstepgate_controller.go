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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/openkruise-operator/api/v1alpha1"
	kruiserolloutv1beta1 "github.com/openkruise/kruise-rollout-api/rollouts/v1beta1"
)

const (
	// Annotation keys for step configuration (user-set)
	annotationStepMaxWaitPrefix             = "rollout.kuberik.io/step-%d-max-wait"
	annotationStepMinWaitAfterSuccessPrefix = "rollout.kuberik.io/step-%d-min-wait-after-success"

	// Internal annotation keys (controller-managed)
	// Store when the step became ready for approval (step paused AND all tests passed)
	internalAnnotationStepReadyAtPrefix = "internal.rollout.kuberik.io/step-%d-ready-at"

	// Large duration to effectively pause indefinitely (max int32, ~68 years in seconds)
	maxPauseDuration = int32(math.MaxInt32)
)

// RolloutStepGateReconciler reconciles Rollout steps with auto-approval logic
type RolloutStepGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rollouts.kruise.io,resources=rollouts/status,verbs=get
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests,verbs=get;list;watch

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

	// Check if step is actually paused (in StepPaused state)
	isStepPaused := r.isStepPaused(&rollout, currentStepIndex)

	// Get all RolloutTests for this step
	tests, err := r.getRolloutTestsForStep(ctx, rollout.Namespace, rollout.Name, currentStepIndex)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Filter tests that match the current canary revision
	currentRevision := rollout.Status.CanaryStatus.CanaryRevision
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
		log.Info("Step max-wait exceeded, keeping paused", "step", currentStepIndex, "deadline", deadline)
		// Keep paused - user needs to intervene
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
