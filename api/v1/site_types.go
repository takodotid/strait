package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Site names the local site. A cluster holds exactly one Site.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gateway.activePod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Site struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:default={}
	Spec   SiteSpec   `json:"spec,omitzero"`
	Status SiteStatus `json:"status,omitempty"`
}

// SiteSpec sets the gateway's ports, the exchange's timing and the site's WireGuard identity.
type SiteSpec struct {
	// Gateway sets the ports of the site's gateway.
	// +kubebuilder:default={}
	// +optional
	Gateway GatewaySpec `json:"gateway,omitzero"`

	// Exchange sets how peers are polled.
	// +kubebuilder:default={}
	// +optional
	Exchange ExchangeSpec `json:"exchange,omitzero"`

	// WireGuard is the site's WireGuard identity. Required when a SiteLink uses the wireguard transport.
	// +optional
	WireGuard *SiteWireGuard `json:"wireguard,omitempty"`
}

// GatewaySpec sets the gateway's ports.
// +kubebuilder:validation:XValidation:rule="!has(self.exportPorts) || !has(self.controlPort) || self.exportPorts.first > self.controlPort || self.exportPorts.last < self.controlPort",message="controlPort must be outside exportPorts"
// +kubebuilder:validation:XValidation:rule="!has(self.importPorts) || !has(self.exportPorts) || self.importPorts.first > self.exportPorts.last || self.importPorts.last < self.exportPorts.first",message="importPorts must not overlap exportPorts"
// +kubebuilder:validation:XValidation:rule="!has(self.importPorts) || !has(self.peerPort) || self.importPorts.first > self.peerPort || self.importPorts.last < self.peerPort",message="peerPort must be outside importPorts"
type GatewaySpec struct {
	// ControlPort serves the observation document and export health to peers.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=30900
	// +optional
	ControlPort int32 `json:"controlPort,omitempty"`

	// ExportPorts is the range that export ports are allocated from. Peers reach exports on these ports.
	// +kubebuilder:default={first:30901,last:31899}
	// +optional
	ExportPorts PortRange `json:"exportPorts,omitzero"`

	// ImportPorts is the range that local import listeners are allocated from. Peers never reach them.
	// +kubebuilder:default={first:15000,last:15999}
	// +optional
	ImportPorts PortRange `json:"importPorts,omitzero"`

	// PeerPort is the local listener that the controller polls peers through. Peers never reach it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=14999
	// +optional
	PeerPort int32 `json:"peerPort,omitempty"`

	// DirectService sets how peers reach the control port and the export ports over direct links.
	// It is ClusterIP unless set: a NodePort or LoadBalancer Service carries the export ports in clear.
	// A site with a direct link sets NodePort or an internal LoadBalancer, exposed on the private link only.
	// +kubebuilder:default={type: ClusterIP}
	// +optional
	DirectService ServiceExposure `json:"directService,omitzero"`

	// WireGuardService sets how peers reach the UDP ports of wireguard links. It is NodePort unless set.
	// +kubebuilder:default={type: NodePort}
	// +optional
	WireGuardService ServiceExposure `json:"wireGuardService,omitzero"`
}

// ServiceExposure sets the type and annotations of a gateway Service.
// With NodePort, each node port equals its port, so a port number is the same on every path.
type ServiceExposure struct {
	// Type is ClusterIP, NodePort or LoadBalancer. Empty means the default of the Service it sets.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type string `json:"type,omitempty"`

	// Annotations are added to the Service, for example to make a load balancer internal.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PortRange is an inclusive range of ports.
// +kubebuilder:validation:XValidation:rule="self.first <= self.last",message="first must not exceed last"
type PortRange struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	First int32 `json:"first"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Last int32 `json:"last"`
}

// ExchangeSpec sets how peers are polled.
// +kubebuilder:validation:XValidation:rule="!has(self.peerTimeout) || !has(self.pollInterval) || duration(self.peerTimeout) >= duration(self.pollInterval) + duration(self.pollInterval)",message="peerTimeout must be at least twice pollInterval"
type ExchangeSpec struct {
	// PollInterval is the time between two polls of a peer.
	// +kubebuilder:default="2s"
	// +optional
	PollInterval metav1.Duration `json:"pollInterval,omitzero"`

	// PeerTimeout is the age after which a peer's last document is stale and its members are Unknown.
	// +kubebuilder:default="6s"
	// +optional
	PeerTimeout metav1.Duration `json:"peerTimeout,omitzero"`
}

// SiteWireGuard is the site's WireGuard identity.
type SiteWireGuard struct {
	// PrivateKeySecretRef names the Secret, in the gateway's namespace, that holds the private key.
	PrivateKeySecretRef SecretKeyRef `json:"privateKeySecretRef"`
}

// SecretKeyRef names one key of a Secret.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:default=privateKey
	// +optional
	Key string `json:"key,omitempty"`
}

// SiteStatus shows the gateway, the site's public key and the controller's version.
type SiteStatus struct {
	// Gateway shows the active gateway replica.
	// +optional
	Gateway GatewayStatus `json:"gateway,omitzero"`

	// WireGuardPublicKey is the public key of the site's private key. Peers put it in their SitePeer.
	// +optional
	WireGuardPublicKey string `json:"wireGuardPublicKey,omitempty"`

	// ControllerVersion is the version of the elected controller.
	// +optional
	ControllerVersion string `json:"controllerVersion,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GatewayStatus shows the active gateway replica.
type GatewayStatus struct {
	// ActivePod is the gateway pod that holds the gateway lease.
	// +optional
	ActivePod string `json:"activePod,omitempty"`
	// Address is the active pod's address.
	// +optional
	Address string `json:"address,omitempty"`
}

// +kubebuilder:object:root=true

// SiteList is a list of Site.
type SiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Site `json:"items"`
}

// SitePeer is another site that this site exchanges observations and traffic with. Its name is the peer's Site name.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Reachable",type=boolean,JSONPath=`.status.reachable`
// +kubebuilder:printcolumn:name="Last Seen",type=date,JSONPath=`.status.lastSeenTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SitePeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:default={}
	Spec   SitePeerSpec   `json:"spec,omitzero"`
	Status SitePeerStatus `json:"status,omitempty"`
}

// SitePeerSpec holds the peer's identity.
type SitePeerSpec struct {
	// ControlPort is the peer gateway's control port, as set in the peer's Site.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=30900
	// +optional
	ControlPort int32 `json:"controlPort,omitempty"`

	// WireGuard holds the peer's public key. Required when a SiteLink to this peer uses the wireguard transport.
	// +optional
	WireGuard *PeerWireGuard `json:"wireguard,omitempty"`
}

// PeerWireGuard holds a peer's WireGuard public key.
type PeerWireGuard struct {
	// PublicKey is the peer site's WireGuard public key, base64.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$`
	PublicKey string `json:"publicKey"`
}

// SitePeerStatus shows whether the peer's observation document can be read.
type SitePeerStatus struct {
	// Reachable is true while the peer's last document is fresh.
	// +optional
	Reachable bool `json:"reachable"`
	// LastSeenTime is the time of the last successful poll.
	// +optional
	LastSeenTime *metav1.Time `json:"lastSeenTime,omitempty"`
	// LastError is the error of the last failed poll.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// Services is the number of SiteServices in the peer's last document.
	// +optional
	Services int32 `json:"services,omitempty"`
	// Exports is the number of exports in the peer's last document.
	// +optional
	Exports int32 `json:"exports,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// SitePeerList is a list of SitePeer.
type SitePeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SitePeer `json:"items"`
}

// Transport is how a link carries traffic.
// +kubebuilder:validation:Enum=direct;wireguard
type Transport string

const (
	// TransportDirect is a private path with no tunnel.
	TransportDirect Transport = "direct"
	// TransportWireGuard is a WireGuard tunnel.
	TransportWireGuard Transport = "wireguard"
)

// Encryption is how a link encrypts traffic.
// +kubebuilder:validation:Enum=None;WireGuard
type Encryption string

const (
	// EncryptionNone carries traffic in clear. Allowed on direct links only, and only when declared.
	EncryptionNone Encryption = "None"
	// EncryptionWireGuard encrypts traffic in a WireGuard tunnel.
	EncryptionWireGuard Encryption = "WireGuard"
)

// SiteLink is one network path between this site and a peer.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Peer",type=string,JSONPath=`.spec.peer`
// +kubebuilder:printcolumn:name="Transport",type=string,JSONPath=`.spec.transport`
// +kubebuilder:printcolumn:name="Cost",type=integer,JSONPath=`.spec.cost`
// +kubebuilder:printcolumn:name="Healthy",type=boolean,JSONPath=`.status.healthy`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
type SiteLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteLinkSpec   `json:"spec"`
	Status SiteLinkStatus `json:"status,omitempty"`
}

// SiteLinkSpec sets a link's peer, transport, cost and addresses.
// +kubebuilder:validation:XValidation:rule="self.transport == 'direct' ? (has(self.direct) && !has(self.wireguard)) : (has(self.wireguard) && !has(self.direct))",message="set direct for the direct transport, wireguard for the wireguard transport, and not the other"
// +kubebuilder:validation:XValidation:rule="self.transport != 'direct' || (has(self.encryption) && self.encryption == 'None')",message="a direct link carries traffic in clear: declare encryption: None"
// +kubebuilder:validation:XValidation:rule="self.transport != 'wireguard' || !has(self.encryption) || self.encryption == 'WireGuard'",message="a wireguard link is encrypted by WireGuard"
type SiteLinkSpec struct {
	// Peer is the name of the SitePeer at the other end.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Peer string `json:"peer"`

	// Transport is how the link carries traffic.
	Transport Transport `json:"transport"`

	// Cost orders the links to one peer. Traffic uses the healthy link with the lowest cost, then the lowest name.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=100
	// +optional
	Cost int32 `json:"cost,omitempty"`

	// Encryption must be None on a direct link, set explicitly. It is WireGuard on a wireguard link.
	// +optional
	Encryption Encryption `json:"encryption,omitempty"`

	// Direct holds the settings of a direct link.
	// +optional
	Direct *DirectLink `json:"direct,omitempty"`

	// WireGuard holds the settings of a wireguard link.
	// +optional
	WireGuard *WireGuardLink `json:"wireguard,omitempty"`
}

// DirectLink holds the settings of a direct link.
type DirectLink struct {
	// Address is the peer gateway's IP address on this link.
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be an IP address"
	Address string `json:"address"`

	// EndpointDirect points application names straight at the remote member's addresses while this link is the active link, with no gateway hop. Pod addresses must be routable over the link.
	// +optional
	EndpointDirect bool `json:"endpointDirect,omitempty"`
}

// WireGuardLink holds the settings of a wireguard link.
type WireGuardLink struct {
	// ListenPort is the local UDP port of this link's interface.
	// With a NodePort or LoadBalancer WireGuard Service, the node port equals it, so it must be in the cluster's node port range.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=31820
	// +optional
	ListenPort int32 `json:"listenPort,omitempty"`

	// Endpoint is the peer's host:port to dial. Empty means this side waits for the peer to dial.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Address is this side's tunnel address, in CIDR notation.
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be an address in CIDR notation"
	Address string `json:"address"`

	// PeerAddress is the peer's tunnel address.
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be an IP address"
	PeerAddress string `json:"peerAddress"`

	// PersistentKeepalive keeps the path open from the dialing side.
	// +kubebuilder:default="25s"
	// +optional
	PersistentKeepalive *metav1.Duration `json:"persistentKeepalive,omitempty"`
}

// SiteLinkStatus shows a link's health as the gateway sees it.
type SiteLinkStatus struct {
	// Healthy is true while the peer's control port answers over this link.
	// +optional
	Healthy bool `json:"healthy"`
	// Active is true while this link carries the pair's traffic.
	// +optional
	Active bool `json:"active"`
	// LastTransitionTime is the last time Healthy changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// Interface is the WireGuard interface of this link.
	// +optional
	Interface string `json:"interface,omitempty"`
	// LastHandshakeTime is the WireGuard peer's last handshake.
	// +optional
	LastHandshakeTime *metav1.Time `json:"lastHandshakeTime,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// SiteLinkList is a list of SiteLink.
type SiteLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteLink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Site{}, &SiteList{}, &SitePeer{}, &SitePeerList{}, &SiteLink{}, &SiteLinkList{})
}
