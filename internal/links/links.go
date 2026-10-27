// Package links orders the links to each peer, as RFC#0005 requirement 2 defines.
package links

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"slices"

	"github.com/takodotid/strait/api/v1alpha1"
)

// ByPeer returns each peer's links, ordered by cost and then by name. The index of a link is its priority.
func ByPeer(links []v1alpha1.SiteLink) map[string][]v1alpha1.SiteLink {
	out := map[string][]v1alpha1.SiteLink{}
	for _, l := range links {
		out[l.Spec.Peer] = append(out[l.Spec.Peer], l)
	}
	for _, ls := range out {
		slices.SortFunc(ls, func(a, b v1alpha1.SiteLink) int {
			return cmp.Or(cmp.Compare(a.Spec.Cost, b.Spec.Cost), cmp.Compare(a.Name, b.Name))
		})
	}
	return out
}

// Address returns the peer's address on the link: the direct address, or the peer's tunnel address.
func Address(l v1alpha1.SiteLink) string {
	switch {
	case l.Spec.Transport == v1alpha1.TransportDirect && l.Spec.Direct != nil:
		return l.Spec.Direct.Address
	case l.Spec.Transport == v1alpha1.TransportWireGuard && l.Spec.WireGuard != nil:
		return l.Spec.WireGuard.PeerAddress
	}
	return ""
}

// Interface returns the name of a wireguard link's interface.
func Interface(linkName string) string {
	h := fnv.New32a()
	h.Write([]byte(linkName))
	return fmt.Sprintf("st%08x", h.Sum32())
}
