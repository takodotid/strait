package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Export offers a local Service port to peers on a gateway port.
// The gateway always forwards an export's traffic to that local Service, never to a SiteService.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.service.name`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.service.port`
// +kubebuilder:printcolumn:name="Gateway Port",type=integer,JSONPath=`.status.gatewayPort`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyEndpoints`
type Export struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExportSpec   `json:"spec"`
	Status ExportStatus `json:"status,omitempty"`
}

// ExportSpec names the local Service port to offer.
type ExportSpec struct {
	// Service is the local Service, in the Export's namespace, and its port.
	Service ServicePortRef `json:"service"`
}

// ServicePortRef names one port of a Service in the same namespace.
type ServicePortRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Port is the Service port number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ExportStatus shows the export's gateway port and readiness.
type ExportStatus struct {
	// GatewayPort is the port that peers reach this export on. Allocated from the Site's exportPorts and kept.
	// +optional
	GatewayPort int32 `json:"gatewayPort,omitempty"`
	// ReadyEndpoints is the number of ready endpoints of the Service.
	// +optional
	ReadyEndpoints int32 `json:"readyEndpoints"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ExportList is a list of Export.
type ExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Export `json:"items"`
}

// Import offers a peer's export locally on a gateway port.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=`.spec.site`
// +kubebuilder:printcolumn:name="Export",type=string,JSONPath=`.spec.export`
// +kubebuilder:printcolumn:name="Gateway Port",type=integer,JSONPath=`.status.gatewayPort`
// +kubebuilder:printcolumn:name="Remote Port",type=integer,JSONPath=`.status.remotePort`
type Import struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImportSpec   `json:"spec"`
	Status ImportStatus `json:"status,omitempty"`
}

// ImportSpec names a peer's export.
type ImportSpec struct {
	// Site is the SitePeer that holds the export.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Site string `json:"site"`
	// Export is the name of the Export at that site, in the same namespace as this Import.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Export string `json:"export"`
}

// ImportStatus shows the import's local gateway port and the peer's export port.
type ImportStatus struct {
	// GatewayPort is the local gateway port of this import. Allocated from the Site's importPorts and kept.
	// +optional
	GatewayPort int32 `json:"gatewayPort,omitempty"`
	// RemotePort is the export's gateway port at the peer, from the peer's last document.
	// +optional
	RemotePort int32 `json:"remotePort,omitempty"`
	// RemoteReady is the export's readiness at the peer, from the peer's last document.
	// +optional
	RemoteReady bool `json:"remoteReady"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ImportList is a list of Import.
type ImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Import `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Export{}, &ExportList{}, &Import{}, &ImportList{})
}
