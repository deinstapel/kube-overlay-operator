/*
Copyright 2022.

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

// OverlayNetworkSpec defines the desired state of OverlayNetwork
type OverlayNetworkSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// AllocatableCIDR is the cidr where member and router pods get their IP addresses allocated from
	AllocatableCIDR string `json:"allocatableCIDR,omitempty"`

	// RoutableCIDRs is the list of cidrs that is reachable through the router pods of this network
	RoutableCIDRs []string `json:"routableCIDRs,omitempty"`
}

// OverlayNetworkStatus defines the observed state of OverlayNetwork
type OverlayNetworkStatus struct {
	// Allocations contains all IP addresses that have been handed out to pods from this network
	Allocations []OverlayNetworkIPAllocation `json:"allocations,omitempty"`

	Routers []OverlayNetworkRouter `json:"routers,omitempty"`
}

// OverlayNetworkIPAllocation contains information on a single IP address and to which pod it belongs
type OverlayNetworkIPAllocation struct {
	PodName string `json:"podName,omitempty"`
	PodIP   string `json:"podIP,omitempty"`
	IP      string `json:"ip,omitempty"`
}

// OverlayNetworkRouter contains information regarding the routers in the network.
type OverlayNetworkRouter struct {
	PodName string `json:"podName,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// OverlayNetwork is the Schema for the overlaynetworks API
type OverlayNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OverlayNetworkSpec   `json:"spec,omitempty"`
	Status OverlayNetworkStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OverlayNetworkList contains a list of OverlayNetwork
type OverlayNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OverlayNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OverlayNetwork{}, &OverlayNetworkList{})
}
