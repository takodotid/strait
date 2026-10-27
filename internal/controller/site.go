// Package controller holds Strait's reconcilers.
package controller

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/exchange"
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

// Peers is what the controller reads from the observation exchange. exchange.Poller implements it.
type Peers interface {
	// Fresh reports whether the peer's last document is fresh.
	Fresh(peer string, now time.Time, timeout time.Duration) bool
	// View returns, for each fresh peer, its declaration and the observations of its own members of one SiteService.
	View(namespace, name string, members []v1alpha1.Member, now time.Time, timeout time.Duration) map[string]observe.Report
	// Snapshot returns the peer's last good document, fresh or not.
	Snapshot(peer string) (exchange.Document, exchange.PeerState)
}

// Local is where the controller publishes the local site's observations.
type Local interface {
	SetService(namespace, name, declared string, members []observe.Observation)
	DeleteService(namespace, name string)
	SetExport(namespace, name string, port int32, ready bool)
	DeleteExport(namespace, name string)
}
