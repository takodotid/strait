// Package controller holds Strait's reconcilers.
package controller

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

// LabelManagedBy marks the EndpointSlices that Strait writes.
const (
	LabelManagedBy   = "endpointslice.kubernetes.io/managed-by"
	ManagedBy        = "strait.tako.id"
	LabelSiteService = "strait.tako.id/siteservice"
	LabelMember      = "strait.tako.id/member"
)

// LocalSite returns the cluster's one Site.
func LocalSite(ctx context.Context, r client.Reader) (*v1alpha1.Site, error) {
	var sites v1alpha1.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return nil, err
	}
	if len(sites.Items) != 1 {
		return nil, fmt.Errorf("want exactly one Site, found %d", len(sites.Items))
	}
	return &sites.Items[0], nil
}

// PeerService is what one fresh peer reports about one SiteService.
type PeerService struct {
	Declared string
	// Members holds the peer's observations of its own members.
	Members map[string]observe.Observation
}

// Peers is what the controller reads from the observation exchange.
type Peers interface {
	// Fresh reports whether the peer's last document is fresh.
	Fresh(peer string, now time.Time, timeout time.Duration) bool
	// Service returns, for each fresh peer that lists the SiteService, its declaration and the observations of its own members.
	Service(namespace, name string, members []v1alpha1.Member, now time.Time, timeout time.Duration) map[string]PeerService
	// Export returns an export's port and readiness from a fresh peer's document.
	Export(peer, namespace, name string, now time.Time, timeout time.Duration) (port int32, ready bool, ok bool)
}

// Local is where the controller publishes the local site's observations.
type Local interface {
	SetService(namespace, name, declared string, members []observe.Observation)
	DeleteService(namespace, name string)
	SetExport(namespace, name string, port int32, ready bool)
	DeleteExport(namespace, name string)
}
