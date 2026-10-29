package plugin

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

// Declared takes the active member from the SiteService's spec, and readiness from the member's Service.
type Declared struct{}

// Observe implements Plugin.
func (Declared) Observe(ctx context.Context, r client.Reader, svc *straitv1.SiteService, site string, now time.Time) observe.Report {
	active := svc.Spec.Plugin.Declared.Active
	rep := observe.Report{Declared: active, Members: map[string]observe.Observation{}}
	for _, m := range LocalMembers(svc, site) {
		o := observe.Observation{Member: m.Name, Site: site, Role: straitv1.RoleReplica, State: straitv1.StateStable, ObservedAt: now}
		if m.Name == active {
			o.Role = straitv1.RolePrimary
		}
		ep, err := ReadyEndpoint(ctx, r, svc.Namespace, m.Service.Name, m.Service.Port)
		switch {
		case errors.Is(err, ErrNoService):
			o.Reason = "ServiceNotFound"
		case err != nil:
			o = observe.Unknown(m.Name, site, "ReadFailed: "+err.Error(), now)
		case len(ep.Addresses) == 0:
			o.Reason = "NoReadyEndpoint"
		default:
			o.Eligible = true
			o.Reason = "Declared"
			if o.Role == straitv1.RoleReplica {
				o.Reason = "Ready"
			}
		}
		o.Endpoint = ep
		rep.Members[m.Name] = o
	}
	return rep
}
