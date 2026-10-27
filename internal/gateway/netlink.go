//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Netlink is the Kernel of the pod's network namespace, through netlink and wgctrl. It needs NET_ADMIN.
type Netlink struct {
	mu sync.Mutex
	wg *wgctrl.Client
}

// NewNetlink opens the WireGuard control client.
func NewNetlink() (*Netlink, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("open WireGuard control: %w", err)
	}
	return &Netlink{wg: c}, nil
}

// Close releases the WireGuard control client.
func (n *Netlink) Close() error { return n.wg.Close() }

// Devices implements Kernel.
func (n *Netlink) Devices() ([]Device, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ls, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	var out []Device
	for _, l := range ls {
		name := l.Attrs().Name
		if l.Type() != "wireguard" || !strings.HasPrefix(name, Prefix) {
			continue
		}
		dev, err := n.wg.Device(name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		d := Device{
			Name:       name,
			Up:         l.Attrs().Flags&net.FlagUp != 0,
			PrivateKey: PrivateKey(dev.PrivateKey),
			ListenPort: dev.ListenPort,
			Peers:      dev.Peers,
		}
		addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("list addresses of %s: %w", name, err)
		}
		for _, a := range addrs {
			if a.IPNet != nil {
				d.Addresses = append(d.Addresses, prefix(*a.IPNet))
			}
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, ownRoute(l, nil), netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL)
		if err != nil {
			return nil, fmt.Errorf("list routes of %s: %w", name, err)
		}
		for _, r := range routes {
			if r.Dst != nil {
				d.Routes = append(d.Routes, prefix(*r.Dst))
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// ownRoute is a route through the link in the main table, marked static so the agent finds its own routes only.
func ownRoute(l netlink.Link, dst *net.IPNet) *netlink.Route {
	return &netlink.Route{LinkIndex: l.Attrs().Index, Dst: dst, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_STATIC, Scope: netlink.SCOPE_LINK}
}

// Create implements Kernel.
func (n *Netlink) Create(name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	la := netlink.NewLinkAttrs()
	la.Name = name
	return netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: la})
}

// Delete implements Kernel.
func (n *Netlink) Delete(name string) error {
	return n.with(name, func(l netlink.Link) error {
		if l.Type() != "wireguard" {
			return fmt.Errorf("%s is a %s interface, not wireguard", name, l.Type())
		}
		return netlink.LinkDel(l)
	})
}

// Configure implements Kernel.
func (n *Netlink) Configure(name string, cfg wgtypes.Config) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.wg.ConfigureDevice(name, cfg)
}

// AddAddress implements Kernel.
func (n *Netlink) AddAddress(name string, p netip.Prefix) error {
	return n.with(name, func(l netlink.Link) error {
		ipn := ipNet(p)
		return netlink.AddrAdd(l, &netlink.Addr{IPNet: &ipn})
	})
}

// DeleteAddress implements Kernel.
func (n *Netlink) DeleteAddress(name string, p netip.Prefix) error {
	return n.with(name, func(l netlink.Link) error {
		ipn := ipNet(p)
		return netlink.AddrDel(l, &netlink.Addr{IPNet: &ipn})
	})
}

// AddRoute implements Kernel.
func (n *Netlink) AddRoute(name string, dst netip.Prefix) error {
	return n.with(name, func(l netlink.Link) error {
		ipn := ipNet(dst.Masked())
		return netlink.RouteReplace(ownRoute(l, &ipn))
	})
}

// DeleteRoute implements Kernel.
func (n *Netlink) DeleteRoute(name string, dst netip.Prefix) error {
	return n.with(name, func(l netlink.Link) error {
		ipn := ipNet(dst.Masked())
		return netlink.RouteDel(ownRoute(l, &ipn))
	})
}

// Up implements Kernel.
func (n *Netlink) Up(name string) error {
	return n.with(name, netlink.LinkSetUp)
}

func (n *Netlink) with(name string, f func(netlink.Link) error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !strings.HasPrefix(name, Prefix) {
		return fmt.Errorf("interface %s does not start with %s", name, Prefix)
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		var nf netlink.LinkNotFoundError
		if errors.As(err, &nf) {
			return fmt.Errorf("interface %s not found", name)
		}
		return err
	}
	return f(l)
}
