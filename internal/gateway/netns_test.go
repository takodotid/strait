//go:build netns

// These tests run the gateway's WireGuard interfaces in network namespaces, as root in a container: make test-netns.
// The host needs the wireguard kernel module.
package gateway

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// netnsImage runs the test binary, pinned by the digest of its multi-platform index.
const netnsImage = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// inContainer reports whether the test runs in its container. Outside it, it runs the test binary there, as root
// with NET_ADMIN and SYS_ADMIN, and no network but its own, and returns false. The container's network namespace
// forwards IPv4 and IPv6, as a pod's may: a new namespace takes IPv4 forwarding from the host's.
func inContainer(t *testing.T) bool {
	t.Helper()
	if os.Getenv("STRAIT_NETNS_INNER") == "1" {
		return true
	}
	if _, err := os.Stat("/sys/module/wireguard"); err != nil {
		t.Fatal("the wireguard kernel module is not loaded on this host; load it with `sudo modprobe wireguard`")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_ADMIN",
		"--sysctl", "net.ipv4.ip_forward=1", "--sysctl", "net.ipv6.conf.all.forwarding=1", "-e", "STRAIT_NETNS_INNER=1", "-v", exe+":/gateway.test:ro", netnsImage,
		"/gateway.test", "-test.run", "^"+t.Name()+"$", "-test.v", "-test.count=1")
	out, err := cmd.CombinedOutput()
	t.Logf("in the container:\n%s", out)
	if err != nil {
		t.Fatalf("in the container: %v", err)
	}
	return false
}

// in runs f in the network namespace. The calling goroutine must be locked to its thread.
func in(t *testing.T, ns netns.NsHandle, f func()) {
	t.Helper()
	if err := netns.Set(ns); err != nil {
		t.Fatal(err)
	}
	f()
}

// family holds the addresses of the test in one IP family. The gateway's pod address and the address that peers dial
// for WireGuard are apart, as a pod address and a node address are, so that the peer can route the pod address into
// its tunnel. The neighbour stands for another pod of the cluster, which the gateway reaches on its own network, at
// gatewaySide. WireGuard itself runs over IPv4 in both.
type family struct {
	name                   string
	pod                    netip.Prefix
	gatewayTun, peerTun    netip.Prefix
	gatewaySide, neighbour netip.Prefix
	any                    netip.Addr
	listenPort             int
}

var families = []family{
	{"IPv4", netip.MustParsePrefix("192.0.2.1/24"), netip.MustParsePrefix("10.99.0.1/32"), netip.MustParsePrefix("10.99.0.2/32"),
		netip.MustParsePrefix("198.51.100.1/24"), netip.MustParsePrefix("198.51.100.2/24"), netip.IPv4Unspecified(), 51820},
	{"IPv6", netip.MustParsePrefix("2001:db8::1/64"), netip.MustParsePrefix("fd00:99::1/128"), netip.MustParsePrefix("fd00:99::2/128"),
		netip.MustParsePrefix("2001:db8:1::1/64"), netip.MustParsePrefix("2001:db8:1::2/64"), netip.IPv6Unspecified(), 51821},
}

var (
	endpointAddr = netip.MustParsePrefix("192.0.2.3/24")
	underlayAddr = netip.MustParsePrefix("192.0.2.2/24")
)

// The gateway's ports in the test, by what listens there.
const (
	controlPort = 30900
	exportPort  = 30901
	importPort  = 15000
	peerPort    = 14999
	probePort   = 8090
	metricsPort = 8091
	// The neighbour's port, as a database's.
	neighbourPort = 5432
)

// target is an address and a port that the peer dials.
type target struct {
	addr netip.Addr
	port int
}

func (t target) String() string { return netip.AddrPortFrom(t.addr, uint16(t.port)).String() }

// serve accepts connections on the address and answers each with the client's address.
func serve(t *testing.T, addr netip.AddrPort) {
	t.Helper()
	l, err := net.Listen("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap().String())
			c.Close()
		}
	}()
}

// reached reports whether the target answers within a second, and the client address that it saw.
func reached(tg target) (bool, string) {
	c, err := net.DialTimeout("tcp", tg.String(), time.Second)
	if err != nil {
		return false, ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	b, err := io.ReadAll(c)
	return err == nil && len(b) > 0, string(b)
}

func mustLink(t *testing.T, name string) netlink.Link {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// addAddr adds an address with no duplicate address detection, so that an IPv6 address is usable at once.
func addAddr(t *testing.T, l netlink.Link, p netip.Prefix) {
	t.Helper()
	ipn := ipNet(p)
	if err := netlink.AddrAdd(l, &netlink.Addr{IPNet: &ipn, Flags: unix.IFA_F_NODAD}); err != nil {
		t.Fatalf("add %s to %s: %v", p, l.Attrs().Name, err)
	}
}

// host is the prefix of the address alone.
func host(p netip.Prefix) netip.Prefix { return netip.PrefixFrom(p.Addr(), p.Addr().BitLen()) }

func up(t *testing.T, l netlink.Link) {
	t.Helper()
	if err := netlink.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
}

// A peer of a wireguard link that widens its own allowed IPs, and routes the gateway pod's address and another pod's
// into its tunnel, reaches only the gateway's control and export ports, in both IP families, though the gateway's
// namespace forwards (RFC0007 requirement 12; issue #64). The gateway side runs the agent's own Netlink kernel and
// Apply.
func TestWireGuardPeerReachesOnlyControlAndExportPorts(t *testing.T) {
	if !inContainer(t) {
		return
	}
	// Every step runs on this thread, which ends with the test, so no other goroutine runs in these namespaces.
	runtime.LockOSThread()

	// The gateway is the container's own namespace, which forwards.
	gw, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	neighbour, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}

	gwKey, _ := wgtypes.GeneratePrivateKey()
	peerKey, _ := wgtypes.GeneratePrivateKey()

	// The underlay: a veth pair between the namespaces.
	in(t, peer, func() {
		up(t, mustLink(t, "lo"))
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}, PeerName: "gw0"}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetNsFd(mustLink(t, "gw0"), int(gw)); err != nil {
			t.Fatal(err)
		}
		l := mustLink(t, "eth0")
		addAddr(t, l, underlayAddr)
		up(t, l)
	})

	// The neighbour: a veth pair to the gateway, a route back through the gateway, and a listener on its address.
	in(t, neighbour, func() {
		up(t, mustLink(t, "lo"))
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}, PeerName: "nb0"}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetNsFd(mustLink(t, "nb0"), int(gw)); err != nil {
			t.Fatal(err)
		}
		l := mustLink(t, "eth0")
		for _, f := range families {
			addAddr(t, l, f.neighbour)
		}
		up(t, l)
		for _, f := range families {
			if err := netlink.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: f.gatewaySide.Addr().AsSlice()}); err != nil {
				t.Fatalf("default route via %s: %v", f.gatewaySide.Addr(), err)
			}
			serve(t, netip.AddrPortFrom(f.neighbour.Addr(), neighbourPort))
		}
	})

	// The gateway: its pod addresses, its side of the neighbour's network, one link per family, and a listener on each
	// of its ports.
	in(t, gw, func() {
		for _, sysctl := range []string{"ipv4/conf/all/forwarding", "ipv6/conf/all/forwarding"} {
			b, err := os.ReadFile("/proc/sys/net/" + sysctl)
			if err != nil || string(b) != "1\n" {
				t.Fatalf("%s in the gateway's namespace: %q %v, want 1", sysctl, b, err)
			}
		}
		up(t, mustLink(t, "lo"))
		if err := netlink.LinkSetName(mustLink(t, "gw0"), "eth0"); err != nil {
			t.Fatal(err)
		}
		l := mustLink(t, "eth0")
		addAddr(t, l, endpointAddr)
		for _, f := range families {
			addAddr(t, l, f.pod)
		}
		up(t, l)
		nb := mustLink(t, "nb0")
		for _, f := range families {
			addAddr(t, nb, f.gatewaySide)
		}
		up(t, nb)

		k, err := NewNetlink()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { k.Close() })
		var want []Interface
		for _, f := range families {
			want = append(want, Interface{
				Name: "st" + f.name, Link: f.name, PrivateKey: PrivateKey(gwKey), ListenPort: f.listenPort,
				Peer: peerKey.PublicKey(), Address: f.gatewayTun, PeerIP: f.peerTun, Route: true,
			})
		}
		failed, err := Apply(k, want)
		if err != nil || len(failed) > 0 {
			t.Fatalf("Apply: %v %v", err, failed)
		}

		// Envoy's control and export listeners bind every address, of both families here. The import and peer
		// listeners, and the agent's probes and metrics, bind the pod's address.
		for _, p := range []int{controlPort, exportPort} {
			serve(t, netip.AddrPortFrom(netip.IPv6Unspecified(), uint16(p)))
		}
		for _, f := range families {
			for _, p := range []int{importPort, peerPort, probePort, metricsPort} {
				serve(t, netip.AddrPortFrom(f.pod.Addr(), uint16(p)))
			}
		}
	})

	// The peer: its own end of each link, with every address allowed, and the gateway's pod address and the neighbour's
	// routed into it.
	in(t, peer, func() {
		c, err := wgctrl.New()
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i, f := range families {
			name := fmt.Sprintf("wg%d", i)
			if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
				t.Fatal(err)
			}
			keepalive := time.Second
			all := netip.PrefixFrom(f.any, 0)
			if err := c.ConfigureDevice(name, wgtypes.Config{
				PrivateKey: &peerKey,
				Peers: []wgtypes.PeerConfig{{
					PublicKey:                   gwKey.PublicKey(),
					Endpoint:                    &net.UDPAddr{IP: endpointAddr.Addr().AsSlice(), Port: f.listenPort},
					AllowedIPs:                  []net.IPNet{ipNet(all)},
					PersistentKeepaliveInterval: &keepalive,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			l := mustLink(t, name)
			addAddr(t, l, f.peerTun)
			up(t, l)
			for _, dst := range []netip.Prefix{f.gatewayTun, host(f.pod), host(f.neighbour)} {
				ipn := ipNet(dst)
				if err := netlink.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Dst: &ipn, Scope: netlink.SCOPE_LINK}); err != nil {
					t.Fatalf("route %s: %v", dst, err)
				}
			}
		}

		var errs []error
		for _, f := range families {
			// The link works: the control port answers on the gateway's tunnel address, to the peer's.
			control := target{f.gatewayTun.Addr(), controlPort}
			deadline := time.Now().Add(10 * time.Second)
			for {
				ok, from := reached(control)
				if ok && from == f.peerTun.Addr().String() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: the peer never reached %s over the link (answered %v, from %q)", f.name, control, ok, from)
				}
			}
			if ok, from := reached(target{f.gatewayTun.Addr(), exportPort}); !ok || from != f.peerTun.Addr().String() {
				errs = append(errs, fmt.Errorf("%s: export port on %s: reached %v from %q, want reached from %s", f.name, f.gatewayTun.Addr(), ok, from, f.peerTun.Addr()))
			}
			tgs := []target{{f.neighbour.Addr(), neighbourPort}}
			for _, p := range []int{importPort, peerPort, probePort, metricsPort} {
				tgs = append(tgs, target{f.pod.Addr(), p})
			}
			for _, tg := range tgs {
				ok, from := reached(tg)
				t.Logf("%s: %s reached: %v, from %q", f.name, tg, ok, from)
				if ok {
					errs = append(errs, fmt.Errorf("%s: %s reached over the link from %s, want refused", f.name, tg, from))
				}
			}
		}
		if err := errors.Join(errs...); err != nil {
			t.Error(err)
		}
	})
}
