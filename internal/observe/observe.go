// Package observe holds what plugins report and what sites exchange.
package observe

import (
	"time"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Observation is one report about one member.
type Observation struct {
	Member     string
	Site       string
	Role       straitv1.Role
	Eligible   bool
	State      straitv1.State
	Reason     string
	ObservedAt time.Time
	// Endpoint is where the member takes traffic. Set for members observed at their own site.
	Endpoint Endpoint
}

// Writable reports whether the member is a confirmed writable primary.
func (o Observation) Writable() bool {
	return o.State == straitv1.StateStable && o.Eligible && o.Role == straitv1.RolePrimary
}

// Confirmed reports whether the member is Stable and eligible.
func (o Observation) Confirmed() bool {
	return o.State == straitv1.StateStable && o.Eligible
}

// Endpoint is a member's endpoint Service and its ready addresses.
type Endpoint struct {
	// Service is the member's endpoint Service, in the SiteService's namespace.
	Service string
	// ServicePort is the port of that Service.
	ServicePort int32
	// Addresses are the ready addresses of the Service.
	Addresses []string
	// Port is the target port of the Service's port.
	Port int32
}

// Unknown is the observation of a member that could not be read.
func Unknown(member, site, reason string, at time.Time) Observation {
	return Observation{Member: member, Site: site, Role: straitv1.RoleUnknown, State: straitv1.StateUnknown, Reason: reason, ObservedAt: at}
}

// Report is what one site observes about its own members of one SiteService.
type Report struct {
	// Declared is the member the site declares primary. Empty when it declares none.
	Declared string
	// Members are the observations, by member name.
	Members map[string]Observation
}
