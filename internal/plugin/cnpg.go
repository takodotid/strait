package plugin

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

// CNPG observes CloudNativePG clusters.
type CNPG struct{}

// Observe implements Plugin.
func (CNPG) Observe(ctx context.Context, r client.Reader, svc *v1alpha1.SiteService, site string, now time.Time) observe.Report {
	rep := observe.Report{Members: map[string]observe.Observation{}}
	for _, m := range LocalMembers(svc, site) {
		rep.Members[m.Name] = observe.Unknown(m.Name, site, "NotImplemented", now)
	}
	return rep
}
