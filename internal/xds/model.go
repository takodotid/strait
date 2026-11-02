// Package xds builds the gateway's Envoy configuration, as RFC#0005 defines, and serves it over ADS.
package xds

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/links"
	"github.com/takodotid/strait/internal/plugin"
)

// Model is everything the gateway's configuration is built from.
type Model struct {
	ControlPort int32
	PeerPort    int32
	// Controller is the host:port of the controller's observation server.
	Controller string
	Exports    []Export
	Imports    []Import
	Peers      []Peer
}

// Export is a local Service port offered to peers on a gateway port.
type Export struct {
	Namespace string
	Name      string
	Port      int32
	// Addresses are the ready endpoints of the Service, reached on TargetPort.
	Addresses  []string
	TargetPort int32
}

// Import is a peer's export offered locally on a gateway port.
type Import struct {
	Namespace string
	Name      string
	Port      int32
	Peer      string
	Export    string
	// RemotePort is the export's gateway port at the peer. 0 while unknown.
	RemotePort int32
}

// Peer is another site and its links, in rank order.
type Peer struct {
	Name        string
	ControlPort int32
	Links       []Link
}

// Link is one path to a peer.
type Link struct {
	Name     string
	Address  string
	Priority uint32
}

// Collect reads the model from the cluster.
// An export whose endpoints cannot be read has none. A link whose address is not an IP address is left out.
func Collect(ctx context.Context, r client.Reader, controllerAddr string) (Model, error) {
	logger := log.FromContext(ctx)
	var sites straitv1.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return Model{}, err
	}
	if len(sites.Items) != 1 {
		return Model{}, fmt.Errorf("want exactly one Site, found %d", len(sites.Items))
	}
	gw := sites.Items[0].Spec.Gateway
	m := Model{ControlPort: gw.ControlPort, PeerPort: gw.PeerPort, Controller: controllerAddr}

	var exports straitv1.ExportList
	if err := r.List(ctx, &exports); err != nil {
		return Model{}, err
	}
	for _, e := range exports.Items {
		if e.Status.GatewayPort == 0 {
			continue
		}
		x := Export{Namespace: e.Namespace, Name: e.Name, Port: e.Status.GatewayPort}
		ep, err := plugin.ReadyEndpoint(ctx, r, e.Namespace, e.Spec.Service.Name, e.Spec.Service.Port)
		if err == nil {
			for _, a := range ep.Addresses {
				if isIP(a) {
					x.Addresses = append(x.Addresses, a)
				}
			}
			x.TargetPort = ep.Port
		}
		m.Exports = append(m.Exports, x)
	}

	var imports straitv1.ImportList
	if err := r.List(ctx, &imports); err != nil {
		return Model{}, err
	}
	for _, i := range imports.Items {
		if i.Status.GatewayPort == 0 {
			continue
		}
		m.Imports = append(m.Imports, Import{
			Namespace: i.Namespace, Name: i.Name, Port: i.Status.GatewayPort,
			Peer: i.Spec.Site, Export: i.Spec.Export, RemotePort: i.Status.RemotePort,
		})
	}

	var peers straitv1.SitePeerList
	if err := r.List(ctx, &peers); err != nil {
		return Model{}, err
	}
	var siteLinks straitv1.SiteLinkList
	if err := r.List(ctx, &siteLinks); err != nil {
		return Model{}, err
	}
	byPeer := links.ByPeer(siteLinks.Items)
	for _, p := range peers.Items {
		peer := Peer{Name: p.Name, ControlPort: p.Spec.ControlPort}
		for _, l := range byPeer[p.Name] {
			addr := links.Address(l)
			if !isIP(addr) {
				logger.V(1).Info("link left out: its address is not an IP address", "link", l.Name, "address", addr)
				continue
			}
			peer.Links = append(peer.Links, Link{Name: l.Name, Address: addr, Priority: uint32(len(peer.Links))})
		}
		m.Peers = append(m.Peers, peer)
	}

	slices.SortFunc(m.Exports, func(a, b Export) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(m.Imports, func(a, b Import) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(m.Peers, func(a, b Peer) int { return cmp.Compare(a.Name, b.Name) })
	return m, nil
}

func isIP(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}
