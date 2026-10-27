package gateway

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/links"
)

// Prefix starts the name of every interface the agent holds. The agent deletes any WireGuard interface with it that it does not want.
const Prefix = "st"

// PrivateKey is a WireGuard private key that never prints. Every fmt verb and every text encoding shows a placeholder.
type PrivateKey wgtypes.Key

const redacted = "<private key>"

// Format implements fmt.Formatter.
func (PrivateKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalText implements encoding.TextMarshaler.
func (PrivateKey) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Interface is the desired state of one wireguard link's interface.
type Interface struct {
	Name       string
	Link       string
	PrivateKey PrivateKey
	ListenPort int
	Peer       wgtypes.Key
	// Endpoint is nil when the link waits for the peer to dial, or when its endpoint did not resolve.
	Endpoint  *net.UDPAddr
	Keepalive time.Duration
	Address   netip.Prefix
	// PeerIP is the peer's tunnel address as a /32 or /128: the peer's only allowed IP.
	PeerIP netip.Prefix
	// Route is true when Address does not cover PeerIP, so the interface needs a route to it.
	Route bool
}

// Inputs is what the desired state is computed from.
type Inputs struct {
	// Key is the site's private key. It is nil when there is none, and NoKey says why.
	Key   *wgtypes.Key
	NoKey string
	Peers []v1alpha1.SitePeer
	Links []v1alpha1.SiteLink
	// Resolve turns an endpoint's host:port into an address.
	Resolve func(hostport string) (*net.UDPAddr, error)
}

// Desired returns the interface of each wireguard link, ordered by link name.
// Problems holds, per link, why it has no interface or why its endpoint is unset.
func Desired(in Inputs) (want []Interface, problems map[string]string) {
	problems = map[string]string{}
	peers := map[string]v1alpha1.SitePeer{}
	for _, p := range in.Peers {
		peers[p.Name] = p
	}
	ls := slices.Clone(in.Links)
	slices.SortFunc(ls, func(a, b v1alpha1.SiteLink) int { return strings.Compare(a.Name, b.Name) })
	ports := map[int]string{}
	for _, l := range ls {
		if l.Spec.Transport != v1alpha1.TransportWireGuard || l.Spec.WireGuard == nil {
			continue
		}
		w, problem := desired(l, in, peers)
		if problem != "" {
			problems[l.Name] = problem
			continue
		}
		if other, ok := ports[w.ListenPort]; ok {
			problems[l.Name] = fmt.Sprintf("listen port %d is taken by SiteLink %s", w.ListenPort, other)
			continue
		}
		ports[w.ListenPort] = l.Name
		if l.Spec.WireGuard.Endpoint != "" && w.Endpoint == nil {
			problems[l.Name] = fmt.Sprintf("endpoint %s does not resolve", l.Spec.WireGuard.Endpoint)
		}
		want = append(want, w)
	}
	return want, problems
}

func desired(l v1alpha1.SiteLink, in Inputs, peers map[string]v1alpha1.SitePeer) (Interface, string) {
	spec := l.Spec.WireGuard
	if in.Key == nil {
		return Interface{}, cmp.Or(in.NoKey, "the Site has no WireGuard private key")
	}
	if spec.ListenPort == 0 {
		return Interface{}, "listen port is not set"
	}
	peer, ok := peers[l.Spec.Peer]
	switch {
	case !ok:
		return Interface{}, fmt.Sprintf("SitePeer %s not found", l.Spec.Peer)
	case peer.Spec.WireGuard == nil || peer.Spec.WireGuard.PublicKey == "":
		return Interface{}, fmt.Sprintf("SitePeer %s has no WireGuard public key", l.Spec.Peer)
	}
	pub, err := wgtypes.ParseKey(peer.Spec.WireGuard.PublicKey)
	if err != nil {
		return Interface{}, fmt.Sprintf("SitePeer %s has an invalid WireGuard public key", l.Spec.Peer)
	}
	addr, err := netip.ParsePrefix(spec.Address)
	if err != nil {
		return Interface{}, fmt.Sprintf("address %q is not in CIDR notation", spec.Address)
	}
	peerIP, ok := HostAddr(spec.PeerAddress)
	if !ok {
		return Interface{}, fmt.Sprintf("peer address %q is not an IP address", spec.PeerAddress)
	}
	w := Interface{
		Name:       links.Interface(l.Name),
		Link:       l.Name,
		PrivateKey: PrivateKey(*in.Key),
		ListenPort: int(spec.ListenPort),
		Peer:       pub,
		Address:    addr,
		PeerIP:     netip.PrefixFrom(peerIP, peerIP.BitLen()),
		Route:      !addr.Contains(peerIP),
	}
	if spec.Endpoint != "" {
		if spec.PersistentKeepalive != nil {
			w.Keepalive = spec.PersistentKeepalive.Duration
		}
		if in.Resolve != nil {
			if ep, err := in.Resolve(spec.Endpoint); err == nil {
				w.Endpoint = ep
			}
		}
	}
	return w, ""
}

// HostAddr parses an IP address, or a prefix that holds exactly one address.
func HostAddr(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if p, err := netip.ParsePrefix(s); err == nil && p.IsSingleIP() {
		return p.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

// Kernel holds the WireGuard interfaces in the pod's network namespace.
type Kernel interface {
	// Devices returns every WireGuard interface whose name starts with Prefix.
	Devices() ([]Device, error)
	Create(name string) error
	Delete(name string) error
	Configure(name string, cfg wgtypes.Config) error
	AddAddress(name string, p netip.Prefix) error
	DeleteAddress(name string, p netip.Prefix) error
	// AddRoute and DeleteRoute act on routes through the interface in the main table, marked as the agent's own.
	AddRoute(name string, dst netip.Prefix) error
	DeleteRoute(name string, dst netip.Prefix) error
	Up(name string) error
}

// Device is the state of one interface.
type Device struct {
	Name       string
	Up         bool
	PrivateKey PrivateKey
	ListenPort int
	Peers      []wgtypes.Peer
	Addresses  []netip.Prefix
	// Routes are the agent's own routes through the interface.
	Routes []netip.Prefix
}

// Apply makes the kernel hold exactly the wanted interfaces, and changes only what differs.
// Failed holds the error of each link whose interface could not be brought to its desired state.
func Apply(k Kernel, want []Interface) (failed map[string]error, err error) {
	have, err := k.Devices()
	if err != nil {
		return nil, err
	}
	byName := map[string]Device{}
	for _, d := range have {
		byName[d.Name] = d
	}
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w.Name] = true
	}
	var errs []error
	for _, d := range have {
		if !wanted[d.Name] {
			if err := k.Delete(d.Name); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", d.Name, err))
			}
		}
	}
	failed = map[string]error{}
	for _, w := range want {
		d, ok := byName[w.Name]
		if !ok {
			if err := k.Create(w.Name); err != nil {
				failed[w.Link] = fmt.Errorf("create %s: %w", w.Name, err)
				continue
			}
			d = Device{Name: w.Name}
		}
		if err := apply(k, w, d); err != nil {
			failed[w.Link] = fmt.Errorf("%s: %w", w.Name, err)
		}
	}
	return failed, errors.Join(errs...)
}

func apply(k Kernel, w Interface, d Device) error {
	if cfg, ok := config(w, d); ok {
		if err := k.Configure(w.Name, cfg); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	}
	for _, a := range d.Addresses {
		if a != w.Address && !a.Addr().IsLinkLocalUnicast() {
			if err := k.DeleteAddress(w.Name, a); err != nil {
				return fmt.Errorf("delete address %s: %w", a, err)
			}
		}
	}
	if !slices.Contains(d.Addresses, w.Address) {
		if err := k.AddAddress(w.Name, w.Address); err != nil {
			return fmt.Errorf("add address %s: %w", w.Address, err)
		}
	}
	if !d.Up {
		if err := k.Up(w.Name); err != nil {
			return fmt.Errorf("set up: %w", err)
		}
	}
	var routes []netip.Prefix
	if w.Route {
		routes = []netip.Prefix{w.PeerIP}
	}
	for _, r := range d.Routes {
		if !slices.Contains(routes, r) {
			if err := k.DeleteRoute(w.Name, r); err != nil {
				return fmt.Errorf("delete route %s: %w", r, err)
			}
		}
	}
	for _, r := range routes {
		if !slices.Contains(d.Routes, r) {
			if err := k.AddRoute(w.Name, r); err != nil {
				return fmt.Errorf("add route %s: %w", r, err)
			}
		}
	}
	return nil
}

// config returns the WireGuard settings that differ between the device and the desired state.
// It never replaces the wanted peer, so its session and an endpoint learned from the peer's dial stay.
func config(w Interface, d Device) (wgtypes.Config, bool) {
	var cfg wgtypes.Config
	if d.PrivateKey != w.PrivateKey {
		k := wgtypes.Key(w.PrivateKey)
		cfg.PrivateKey = &k
	}
	if d.ListenPort != w.ListenPort {
		p := w.ListenPort
		cfg.ListenPort = &p
	}
	var cur *wgtypes.Peer
	for i, p := range d.Peers {
		if p.PublicKey == w.Peer {
			cur = &d.Peers[i]
			continue
		}
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{PublicKey: p.PublicKey, Remove: true})
	}
	pc := wgtypes.PeerConfig{PublicKey: w.Peer}
	changed := false
	if cur == nil || len(cur.AllowedIPs) != 1 || prefix(cur.AllowedIPs[0]) != w.PeerIP {
		pc.ReplaceAllowedIPs = true
		pc.AllowedIPs = []net.IPNet{ipNet(w.PeerIP)}
		changed = true
	}
	if w.Endpoint != nil && (cur == nil || cur.Endpoint == nil || !cur.Endpoint.IP.Equal(w.Endpoint.IP) || cur.Endpoint.Port != w.Endpoint.Port) {
		pc.Endpoint = w.Endpoint
		changed = true
	}
	if cur == nil || cur.PersistentKeepaliveInterval != w.Keepalive {
		ka := w.Keepalive
		pc.PersistentKeepaliveInterval = &ka
		changed = true
	}
	if changed {
		cfg.Peers = append(cfg.Peers, pc)
	}
	return cfg, cfg.PrivateKey != nil || cfg.ListenPort != nil || len(cfg.Peers) > 0
}

func prefix(n net.IPNet) netip.Prefix {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones)
}

func ipNet(p netip.Prefix) net.IPNet {
	return net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}
