package gateway

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/links"
)

func newKey(t *testing.T) wgtypes.Key {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func peer(name string, pub string) v1alpha1.SitePeer {
	p := v1alpha1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if pub != "" {
		p.Spec.WireGuard = &v1alpha1.PeerWireGuard{PublicKey: pub}
	}
	return p
}

func wgLink(name, peer string, cost int32, port int32, endpoint, addr, peerAddr string) v1alpha1.SiteLink {
	return v1alpha1.SiteLink{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SiteLinkSpec{
			Peer: peer, Transport: v1alpha1.TransportWireGuard, Cost: cost,
			WireGuard: &v1alpha1.WireGuardLink{
				ListenPort: port, Endpoint: endpoint, Address: addr, PeerAddress: peerAddr,
				PersistentKeepalive: &metav1.Duration{Duration: 25 * time.Second},
			},
		},
	}
}

func directLink(name, peer string, cost int32, addr string) v1alpha1.SiteLink {
	return v1alpha1.SiteLink{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.SiteLinkSpec{Peer: peer, Transport: v1alpha1.TransportDirect, Cost: cost, Encryption: v1alpha1.EncryptionNone, Direct: &v1alpha1.DirectLink{Address: addr}},
	}
}

func resolve(hostport string) (*net.UDPAddr, error) {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		return nil, errors.New("no such host")
	}
	return net.UDPAddrFromAddrPort(ap), nil
}

func TestDesired(t *testing.T) {
	site := newKey(t)
	pubB, pubC := newKey(t).PublicKey(), newKey(t).PublicKey()
	in := Inputs{
		Key:   &site,
		Peers: []v1alpha1.SitePeer{peer("b", pubB.String()), peer("c", pubC.String()), peer("d", "")},
		Links: []v1alpha1.SiteLink{
			wgLink("to-b", "b", 100, 51820, "192.0.2.10:51820", "100.64.0.1/32", "100.64.0.2"),
			wgLink("to-c", "c", 100, 51821, "", "100.64.1.1/30", "100.64.1.2"),
			wgLink("to-d", "d", 100, 51822, "", "100.64.2.1/32", "100.64.2.2"),
			wgLink("to-e", "e", 100, 51823, "", "100.64.3.1/32", "100.64.3.2"),
			wgLink("to-c-again", "c", 200, 51821, "", "100.64.4.1/32", "100.64.4.2"),
			wgLink("to-b-dns", "b", 300, 51824, "peer.invalid:51820", "100.64.5.1/32", "100.64.5.2/32"),
			directLink("lan", "b", 10, "198.51.100.2"),
		},
		Resolve: resolve,
	}
	want, problems := Desired(in)

	var names []string
	for _, w := range want {
		names = append(names, w.Link)
	}
	if !slices.Equal(names, []string{"to-b", "to-b-dns", "to-c"}) {
		t.Fatalf("interfaces for %v", names)
	}
	b, dns, c := want[0], want[1], want[2]
	if b.Name != links.Interface("to-b") || b.PrivateKey != PrivateKey(site) || b.Peer != pubB || b.ListenPort != 51820 {
		t.Errorf("to-b: name %s, port %d, keys match %v", b.Name, b.ListenPort, b.PrivateKey == PrivateKey(site) && b.Peer == pubB)
	}
	if b.Endpoint == nil || b.Endpoint.String() != "192.0.2.10:51820" || b.Keepalive != 25*time.Second {
		t.Errorf("to-b: endpoint %v, keepalive %s", b.Endpoint, b.Keepalive)
	}
	if b.PeerIP != netip.MustParsePrefix("100.64.0.2/32") || !b.Route {
		t.Errorf("to-b: peer %s, route %v", b.PeerIP, b.Route)
	}
	if c.Endpoint != nil || c.Keepalive != 0 || c.Route || c.Address != netip.MustParsePrefix("100.64.1.1/30") {
		t.Errorf("to-c: endpoint %v, keepalive %s, route %v, address %s", c.Endpoint, c.Keepalive, c.Route, c.Address)
	}
	if dns.Endpoint != nil || dns.Keepalive != 25*time.Second || dns.PeerIP != netip.MustParsePrefix("100.64.5.2/32") {
		t.Errorf("to-b-dns: endpoint %v, keepalive %s, peer %s", dns.Endpoint, dns.Keepalive, dns.PeerIP)
	}
	for link, want := range map[string]string{
		"to-d":       "SitePeer d has no WireGuard public key",
		"to-e":       "SitePeer e not found",
		"to-c-again": "listen port 51821 is taken by SiteLink to-c",
		"to-b-dns":   "endpoint peer.invalid:51820 does not resolve",
	} {
		if problems[link] != want {
			t.Errorf("problem of %s = %q, want %q", link, problems[link], want)
		}
	}
	if len(problems) != 4 {
		t.Errorf("problems = %v", problems)
	}
}

func TestDesiredWithoutKey(t *testing.T) {
	in := Inputs{
		NoKey: "Secret strait/wg not found",
		Peers: []v1alpha1.SitePeer{peer("b", newKey(t).PublicKey().String())},
		Links: []v1alpha1.SiteLink{wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")},
	}
	want, problems := Desired(in)
	if len(want) != 0 || problems["to-b"] != "Secret strait/wg not found" {
		t.Fatalf("%d interfaces, problems %v", len(want), problems)
	}
}

func TestPrivateKeyNeverPrints(t *testing.T) {
	k := newKey(t)
	w := Interface{PrivateKey: PrivateKey(k)}
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		if out := fmt.Sprintf(f, w); strings.Contains(out, k.String()) || strings.Contains(out, fmt.Sprintf("%x", k[:4])) || !strings.Contains(out, redacted) {
			t.Errorf("%s prints %q", f, out)
		}
	}
}

// fakeKernel holds devices in memory and applies wgtypes.Config the way the kernel does.
type fakeKernel struct {
	mu    sync.Mutex
	devs  map[string]*Device
	calls []string
}

func newFakeKernel() *fakeKernel { return &fakeKernel{devs: map[string]*Device{}} }

func (k *fakeKernel) Devices() ([]Device, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []Device
	for _, n := range slices.Sorted(maps.Keys(k.devs)) {
		d := *k.devs[n]
		d.Peers = slices.Clone(d.Peers)
		d.Addresses = slices.Clone(d.Addresses)
		d.Routes = slices.Clone(d.Routes)
		out = append(out, d)
	}
	return out, nil
}

func (k *fakeKernel) call(f string, args ...any) { k.calls = append(k.calls, fmt.Sprintf(f, args...)) }

func (k *fakeKernel) dev(name string) (*Device, error) {
	d, ok := k.devs[name]
	if !ok {
		return nil, fmt.Errorf("%s not found", name)
	}
	return d, nil
}

func (k *fakeKernel) Create(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.call("create %s", name)
	k.devs[name] = &Device{Name: name}
	return nil
}

func (k *fakeKernel) Delete(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.call("delete %s", name)
	delete(k.devs, name)
	return nil
}

func (k *fakeKernel) Configure(name string, cfg wgtypes.Config) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	var parts []string
	if cfg.PrivateKey != nil {
		d.PrivateKey = PrivateKey(*cfg.PrivateKey)
		parts = append(parts, "key")
	}
	if cfg.ListenPort != nil {
		d.ListenPort = *cfg.ListenPort
		parts = append(parts, fmt.Sprintf("port=%d", *cfg.ListenPort))
	}
	if cfg.ReplacePeers {
		d.Peers = nil
		parts = append(parts, "replace-peers")
	}
	for _, pc := range cfg.Peers {
		i := slices.IndexFunc(d.Peers, func(p wgtypes.Peer) bool { return p.PublicKey == pc.PublicKey })
		if pc.Remove {
			if i >= 0 {
				d.Peers = slices.Delete(d.Peers, i, i+1)
			}
			parts = append(parts, "remove-peer")
			continue
		}
		if i < 0 {
			d.Peers = append(d.Peers, wgtypes.Peer{PublicKey: pc.PublicKey})
			i = len(d.Peers) - 1
			parts = append(parts, "add-peer")
		}
		p := &d.Peers[i]
		if pc.ReplaceAllowedIPs {
			p.AllowedIPs = nil
			parts = append(parts, "allowed-ips")
		}
		p.AllowedIPs = append(p.AllowedIPs, pc.AllowedIPs...)
		if pc.Endpoint != nil {
			p.Endpoint = pc.Endpoint
			parts = append(parts, "endpoint="+pc.Endpoint.String())
		}
		if pc.PersistentKeepaliveInterval != nil {
			p.PersistentKeepaliveInterval = *pc.PersistentKeepaliveInterval
			parts = append(parts, "keepalive="+pc.PersistentKeepaliveInterval.String())
		}
	}
	k.call("configure %s %s", name, strings.Join(parts, " "))
	return nil
}

func (k *fakeKernel) AddAddress(name string, p netip.Prefix) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	k.call("add-address %s %s", name, p)
	d.Addresses = append(d.Addresses, p)
	return nil
}

func (k *fakeKernel) DeleteAddress(name string, p netip.Prefix) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	k.call("delete-address %s %s", name, p)
	d.Addresses = slices.DeleteFunc(d.Addresses, func(a netip.Prefix) bool { return a == p })
	return nil
}

func (k *fakeKernel) AddRoute(name string, dst netip.Prefix) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	if !d.Up {
		return errors.New("network is down")
	}
	k.call("add-route %s %s", name, dst)
	d.Routes = append(d.Routes, dst)
	return nil
}

func (k *fakeKernel) DeleteRoute(name string, dst netip.Prefix) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	k.call("delete-route %s %s", name, dst)
	d.Routes = slices.DeleteFunc(d.Routes, func(r netip.Prefix) bool { return r == dst })
	return nil
}

func (k *fakeKernel) Up(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, err := k.dev(name)
	if err != nil {
		return err
	}
	k.call("up %s", name)
	d.Up = true
	return nil
}

func (k *fakeKernel) takeCalls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	c := k.calls
	k.calls = nil
	return c
}

func TestApply(t *testing.T) {
	site := newKey(t)
	pubB, pubC := newKey(t).PublicKey(), newKey(t).PublicKey()
	in := Inputs{
		Key:   &site,
		Peers: []v1alpha1.SitePeer{peer("b", pubB.String()), peer("c", pubC.String())},
		Links: []v1alpha1.SiteLink{
			wgLink("to-b", "b", 100, 51820, "192.0.2.10:51820", "100.64.0.1/32", "100.64.0.2"),
			wgLink("to-c", "c", 100, 51821, "", "100.64.1.1/30", "100.64.1.2"),
		},
		Resolve: resolve,
	}
	k := newFakeKernel()
	k.devs["st00000000"] = &Device{Name: "st00000000", Up: true}
	ifB, ifC := links.Interface("to-b"), links.Interface("to-c")

	want, _ := Desired(in)
	failed, err := Apply(k, want)
	if err != nil || len(failed) != 0 {
		t.Fatalf("Apply: %v, failed %v", err, failed)
	}
	wantCalls := []string{
		"delete st00000000",
		"create " + ifB,
		"configure " + ifB + " key port=51820 add-peer allowed-ips endpoint=192.0.2.10:51820 keepalive=25s",
		"add-address " + ifB + " 100.64.0.1/32",
		"up " + ifB,
		"add-route " + ifB + " 100.64.0.2/32",
		"create " + ifC,
		"configure " + ifC + " key port=51821 add-peer allowed-ips keepalive=0s",
		"add-address " + ifC + " 100.64.1.1/30",
		"up " + ifC,
	}
	if got := k.takeCalls(); !slices.Equal(got, wantCalls) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}

	if _, err := Apply(k, want); err != nil {
		t.Fatal(err)
	}
	if got := k.takeCalls(); len(got) != 0 {
		t.Fatalf("second Apply changed %v", got)
	}

	// The peer dialed from another address: the waiting side keeps the endpoint it learned.
	learned := &net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 40000}
	k.devs[ifC].Peers[0].Endpoint = learned
	if _, err := Apply(k, want); err != nil {
		t.Fatal(err)
	}
	if got := k.takeCalls(); len(got) != 0 || k.devs[ifC].Peers[0].Endpoint != learned {
		t.Fatalf("Apply touched a learned endpoint: %v", got)
	}

	// Spec changes: c gets a new peer key and address, b drops its endpoint, so it waits and keeps the session.
	newC := newKey(t).PublicKey()
	in.Peers[1] = peer("c", newC.String())
	in.Links[1].Spec.WireGuard.Address = "100.64.1.5/32"
	in.Links[1].Spec.WireGuard.PeerAddress = "100.64.1.6"
	in.Links[0].Spec.WireGuard.Endpoint = ""
	want, _ = Desired(in)
	if _, err := Apply(k, want); err != nil {
		t.Fatal(err)
	}
	wantCalls = []string{
		"configure " + ifB + " keepalive=0s",
		"configure " + ifC + " remove-peer add-peer allowed-ips keepalive=0s",
		"delete-address " + ifC + " 100.64.1.1/30",
		"add-address " + ifC + " 100.64.1.5/32",
		"add-route " + ifC + " 100.64.1.6/32",
	}
	if got := k.takeCalls(); !slices.Equal(got, wantCalls) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}
	if d := k.devs[ifC]; len(d.Peers) != 1 || d.Peers[0].PublicKey != newC {
		t.Fatalf("peers of %s: %d", ifC, len(d.Peers))
	}

	// Without the private key nothing is wanted, and every interface goes.
	in.Key, in.NoKey = nil, "Secret strait/wg not found"
	want, _ = Desired(in)
	if _, err := Apply(k, want); err != nil {
		t.Fatal(err)
	}
	if len(k.devs) != 0 {
		t.Fatalf("left %d interfaces", len(k.devs))
	}
}
