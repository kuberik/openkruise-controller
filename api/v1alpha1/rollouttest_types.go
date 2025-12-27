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

package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RolloutTestSpec defines the desired state of RolloutTest
type RolloutTestSpec struct {
	// RolloutName is the name of the Rollout to watch.
	// +required
	RolloutName string `json:"rolloutName"`

	// StepIndex is the index of the step in the Rollout strategy to execute the test at.
	// +required
	StepIndex int32 `json:"stepIndex"`

	// JobTemplate is the template for the Job to run.
	// +required
	JobTemplate batchv1.JobSpec `json:"jobTemplate"`
}

// RolloutTestPhase represents the current phase of a RolloutTest
// +kubebuilder:validation:Enum=WaitingForStep;Pending;Running;Succeeded;Failed
type RolloutTestPhase string

const (
	// RolloutTestPhaseWaitingForStep indicates the test is waiting for the rollout to reach the target step
	RolloutTestPhaseWaitingForStep RolloutTestPhase = "WaitingForStep"
	// RolloutTestPhasePending indicates the job has been created but hasn't started running yet
	RolloutTestPhasePending RolloutTestPhase = "Pending"
	// RolloutTestPhaseRunning indicates the job is currently running
	RolloutTestPhaseRunning RolloutTestPhase = "Running"
	// RolloutTestPhaseSucceeded indicates the test job completed successfully
	RolloutTestPhaseSucceeded RolloutTestPhase = "Succeeded"
	// RolloutTestPhaseFailed indicates the test job failed
	RolloutTestPhaseFailed RolloutTestPhase = "Failed"
)

// RolloutTestStatus defines the observed state of RolloutTest.
type RolloutTestStatus struct {
	// Conditions store the status conditions of the RolloutTest.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedCanaryRevision is the canaryRevision from the Rollout that the current job was created for.
	// When the canaryRevision changes, it indicates a new rollout and the old job should be deleted.
	// +optional
	ObservedCanaryRevision string `json:"observedCanaryRevision,omitempty"`

	// Phase represents the current phase of the RolloutTest
	// +optional
	Phase RolloutTestPhase `json:"phase,omitempty"`

	// JobName is the name of the Job created for this test
	// +optional
	JobName string `json:"jobName,omitempty"`

	// RetryCount is the number of times the job has been retried (from job status)
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// ActivePods is the number of active pods for the job
	// +optional
	ActivePods int32 `json:"activePods,omitempty"`

	// SucceededPods is the number of succeeded pods for the job
	// +optional
	SucceededPods int32 `json:"succeededPods,omitempty"`

	// FailedPods is the number of failed pods for the job
	// +optional
	FailedPods int32 `json:"failedPods,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RolloutTest is the Schema for the rollouttests API
type RolloutTest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of RolloutTest
	// +required
	Spec RolloutTestSpec `json:"spec"`

	// status defines the observed state of RolloutTest
	// +optional
	Status RolloutTestStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// RolloutTestList contains a list of RolloutTest
type RolloutTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutTest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutTest{}, &RolloutTestList{})
}
