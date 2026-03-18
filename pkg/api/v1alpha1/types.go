/*
Copyright 2024 The HAMi Authors.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GroupName = "hami.io"
	Version  = "v1alpha1"
	Resource = "vgpuscalings"
	Kind     = "VGPUScaling"

	PhasePending    = "Pending"
	PhaseValidating = "Validating"
	PhaseApplying   = "Applying"
	PhaseCompleted  = "Completed"
	PhaseFailed     = "Failed"
)

type GPUResources struct {
	MemoryLimit string `json:"memoryLimit"`
	CoreLimit   int32  `json:"coreLimit"`
}

type VGPUScalingSpec struct {
	TargetPod       string       `json:"targetPod"`
	TargetContainer string       `json:"targetContainer"`
	GPU             GPUResources `json:"gpu"`
	Action          string       `json:"action"` // "scale"
}

type VGPUScalingStatus struct {
	Phase               string `json:"phase,omitempty"`
	Message             string `json:"message,omitempty"`
	PreviousMemoryLimit string `json:"previousMemoryLimit,omitempty"`
	PreviousCoreLimit   int32  `json:"previousCoreLimit,omitempty"`
	AppliedAt           string `json:"appliedAt,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VGPUScaling struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VGPUScalingSpec   `json:"spec,omitempty"`
	Status VGPUScalingStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type VGPUScalingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []VGPUScaling `json:"items"`
}

