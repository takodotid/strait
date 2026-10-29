package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode is how a SiteService picks its active members.
// +kubebuilder:validation:Enum=Single;Set
type Mode string

const (
	// ModeSingle routes to the one writable member.
	ModeSingle Mode = "Single"
	// ModeSet routes to every eligible member.
	ModeSet Mode = "Set"
)

// Locality filters the members of a Set by site.
// +kubebuilder:validation:Enum=LocalFirst;LocalOnly;Any
type Locality string

const (
	// LocalFirst routes to local eligible members if there is one, else to every eligible member.
	LocalFirst Locality = "LocalFirst"
	// LocalOnly routes to local eligible members only.
	LocalOnly Locality = "LocalOnly"
	// Any routes to every eligible member.
	Any Locality = "Any"
)

// MultiplePolicy is what Single mode does when two or more members are writable.
// +kubebuilder:validation:Enum=RouteNowhere;PreferDeclaredIfConsistent
type MultiplePolicy string

const (
	// RouteNowhere leaves the application name with no endpoints.
	RouteNowhere MultiplePolicy = "RouteNowhere"
	// PreferDeclaredIfConsistent routes to the declared primary when every site declares the same one and it is writable.
	PreferDeclaredIfConsistent MultiplePolicy = "PreferDeclaredIfConsistent"
)

// CNPGRole is the CloudNativePG role a SiteService routes to.
// +kubebuilder:validation:Enum=primary;replica;any
type CNPGRole string

const (
	// CNPGRolePrimary routes to the writable primary. Single mode only.
	CNPGRolePrimary CNPGRole = "primary"
	// CNPGRoleReplica routes to read-only instances. Set mode only.
	CNPGRoleReplica CNPGRole = "replica"
	// CNPGRoleAny routes to any instance. Set mode only.
	CNPGRoleAny CNPGRole = "any"
)

// SiteService is one application name. It lists a dependency's members across sites and points a local Service at the active ones.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="the name must be a DNS-1035 label, since it names a Service"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Route",type=string,JSONPath=`.status.route.members`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.route.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SiteService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteServiceSpec   `json:"spec"`
	Status SiteServiceStatus `json:"status,omitempty"`
}

// SiteServiceSpec lists the members, the mode, the plugin and the policy.
// +kubebuilder:validation:XValidation:rule="self.mode == 'Set' || !has(self.locality)",message="locality applies to Set mode only"
// +kubebuilder:validation:XValidation:rule="!has(self.plugin.cnpg) || (self.plugin.cnpg.role == 'primary') == (self.mode == 'Single')",message="cnpg role primary requires Single mode; replica and any require Set mode"
// +kubebuilder:validation:XValidation:rule="!has(self.plugin.declared) || self.mode == 'Set' || has(self.plugin.declared.active)",message="the declared plugin needs active in Single mode"
// +kubebuilder:validation:XValidation:rule="!has(self.plugin.declared) || !has(self.plugin.declared.active) || self.members.exists(m, m.name == self.plugin.declared.active)",message="declared.active must name a member"
// +kubebuilder:validation:XValidation:rule="self.members.all(m, has(self.plugin.cnpg) ? (has(m.cnpg) && !has(m.service)) : (has(m.service) && !has(m.cnpg)))",message="every member must carry the source of the plugin in use, and no other"
// +kubebuilder:validation:XValidation:rule="self.members.all(m, self.members.exists_one(n, n.name == m.name))",message="member names must be unique"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Single' || !has(self.policy.startupHold) || duration(self.policy.startupHold) == duration('0s')",message="startupHold applies to Single mode only"
type SiteServiceSpec struct {
	// Mode is Single, for one writable member, or Set, for every eligible member.
	Mode Mode `json:"mode"`

	// Locality filters a Set's members by site. Set mode only. Defaults to LocalFirst.
	// +optional
	Locality Locality `json:"locality,omitempty"`

	// Port is the port of the application name.
	Port ServicePort `json:"port"`

	// Plugin is the plugin that observes the members.
	Plugin PluginSpec `json:"plugin"`

	// Members are the dependency's members, at every site.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Members []Member `json:"members"`

	// Policy decides the route when observations are incomplete or contradict each other.
	// +kubebuilder:default={}
	// +optional
	Policy Policy `json:"policy,omitzero"`
}

// ServicePort is the one port of a Service that Strait writes: an application name, or an Import's Service.
type ServicePort struct {
	// Name is the port's name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=15
	Name string `json:"name"`
	// Port is the port number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PluginSpec names exactly one plugin.
// +kubebuilder:validation:XValidation:rule="has(self.cnpg) != has(self.declared)",message="set exactly one plugin"
type PluginSpec struct {
	// CNPG observes CloudNativePG clusters.
	// +optional
	CNPG *CNPGPlugin `json:"cnpg,omitempty"`
	// Declared takes the active member from this spec.
	// +optional
	Declared *DeclaredPlugin `json:"declared,omitempty"`
}

// CNPGPlugin sets the role the cnpg plugin routes to.
type CNPGPlugin struct {
	// Role is primary, replica or any.
	Role CNPGRole `json:"role"`
}

// DeclaredPlugin names the active member.
type DeclaredPlugin struct {
	// Active is the member that is primary. Required in Single mode.
	// +optional
	Active string `json:"active,omitempty"`
}

// Member is one instance of the dependency at one site.
type Member struct {
	// Name identifies the member. The same at every site.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Site is the name of the site that hosts the member.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Site string `json:"site"`

	// CNPG is the member's CloudNativePG cluster, in the SiteService's namespace.
	// +optional
	CNPG *CNPGMember `json:"cnpg,omitempty"`

	// Service is the member's Service, in the SiteService's namespace. For the declared plugin.
	// +optional
	Service *ServicePortRef `json:"service,omitempty"`
}

// CNPGMember is one CloudNativePG cluster.
type CNPGMember struct {
	// Cluster is the name of the CloudNativePG Cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50
	Cluster string `json:"cluster"`
	// Pooler is the name of a CloudNativePG Pooler of that cluster. When set, the member's endpoint is the Pooler's Service.
	// +optional
	Pooler string `json:"pooler,omitempty"`
}

// Policy decides the route when observations are incomplete or contradict each other.
// +kubebuilder:validation:XValidation:rule="!has(self.unknownHold) || (duration(self.unknownHold) >= duration('0s') && duration(self.unknownHold) <= duration('10m'))",message="unknownHold must be between 0s and 10m"
// +kubebuilder:validation:XValidation:rule="!has(self.startupHold) || (duration(self.startupHold) >= duration('0s') && duration(self.startupHold) <= duration('10m'))",message="startupHold must be between 0s and 10m"
type Policy struct {
	// Multiple is what Single mode does when two or more members are writable.
	// +kubebuilder:default=RouteNowhere
	// +optional
	Multiple MultiplePolicy `json:"multiple,omitempty"`

	// UnknownHold is how long an Unknown member stays in the route after it was last confirmed.
	// +kubebuilder:default="30s"
	// +optional
	UnknownHold metav1.Duration `json:"unknownHold,omitzero"`

	// StartupHold keeps Single mode from routing to a local member after the controller starts, until every site
	// with a member has been read once, or until this much time has passed. Unset or 0s turns it off.
	// +optional
	StartupHold metav1.Duration `json:"startupHold,omitzero"`
}

// State is a member's state.
type State string

const (
	// StateStable means the member was read and is not moving.
	StateStable State = "Stable"
	// StateTransitioning means the member is moving.
	StateTransitioning State = "Transitioning"
	// StateUnknown means the member could not be read.
	StateUnknown State = "Unknown"
)

// Role is a member's role.
type Role string

const (
	// RolePrimary accepts writes.
	RolePrimary Role = "primary"
	// RoleReplica is read-only.
	RoleReplica Role = "replica"
	// RoleUnknown is not known.
	RoleUnknown Role = "unknown"
)

// Path is how a routed member takes traffic from this site.
// +kubebuilder:validation:Enum=Local;Gateway;EndpointDirect
type Path string

const (
	// PathLocal is the member's own pods at this site.
	PathLocal Path = "Local"
	// PathGateway is the local gateway, over the active link to the member's site.
	PathGateway Path = "Gateway"
	// PathEndpointDirect is the member's own pods at its site, over a direct link with endpointDirect.
	PathEndpointDirect Path = "EndpointDirect"
)

// Reasons of a route.
const (
	ReasonDeclarationsDisagree = "DeclarationsDisagree"
	ReasonPreferDeclared       = "PreferDeclared"
	ReasonMultipleWritable     = "MultipleWritable"
	ReasonNotDeclared          = "NotDeclared"
	ReasonActive               = "Active"
	ReasonTransitioning        = "Transitioning"
	ReasonHeldUnknown          = "HeldUnknown"
	ReasonUnknownTimeout       = "UnknownTimeout"
	ReasonMemberUnknown        = "MemberUnknown"
	ReasonNoEligibleMember     = "NoEligibleMember"
	ReasonEligible             = "Eligible"
	ReasonAwaitingPeers        = "AwaitingPeers"
)

// Condition types of a SiteService.
const (
	ConditionRouted               = "Routed"
	ConditionMultiple             = "Multiple"
	ConditionDeclarationsDisagree = "DeclarationsDisagree"
	ConditionHeld                 = "Held"
)

// SiteServiceStatus shows the route and every input of the decision.
type SiteServiceStatus struct {
	// ObservedGeneration is the generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Route is the current route.
	// +optional
	Route Route `json:"route,omitempty"`

	// Members shows each member's last observation.
	// +listType=map
	// +listMapKey=name
	// +optional
	Members []MemberStatus `json:"members,omitempty"`

	// Declarations shows each known site's declared primary.
	// +listType=map
	// +listMapKey=site
	// +optional
	Declarations []Declaration `json:"declarations,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Route is the set of routed members and why.
type Route struct {
	// Members are the routed members. Empty means the route is nowhere.
	// +optional
	Members []string `json:"members,omitempty"`
	// Reason is the decision's reason.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains the reason.
	// +optional
	Message string `json:"message,omitempty"`
	// LastChangeTime is the last time Members changed.
	// +optional
	LastChangeTime *metav1.Time `json:"lastChangeTime,omitempty"`
	// Endpoints is the number of endpoints written for the application name.
	// +optional
	Endpoints int32 `json:"endpoints"`
}

// MemberStatus is a member's last observation.
type MemberStatus struct {
	Name string `json:"name"`
	Site string `json:"site"`
	// +optional
	Role Role `json:"role,omitempty"`
	// +optional
	Eligible bool `json:"eligible"`
	// +optional
	State State `json:"state,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// ObservedTime is when the observation was made.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
	// LastConfirmedTime is the last time the member was observed Stable and eligible.
	// +optional
	LastConfirmedTime *metav1.Time `json:"lastConfirmedTime,omitempty"`
	// Path is how the member takes traffic while it is routed and has endpoints. Empty otherwise.
	// +optional
	Path Path `json:"path,omitempty"`
}

// Declaration is a site's declared primary.
type Declaration struct {
	Site string `json:"site"`
	// Primary is the member the site declares primary. Empty when the site declares none.
	// +optional
	Primary string `json:"primary,omitempty"`
}

// +kubebuilder:object:root=true

// SiteServiceList is a list of SiteService.
type SiteServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SiteService{}, &SiteServiceList{})
}
