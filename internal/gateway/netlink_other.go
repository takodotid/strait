//go:build !linux

package gateway

import (
	"errors"
	"net/netip"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

var errLinux = errors.New("WireGuard interfaces need Linux")

// Netlink is the Kernel of the pod's network namespace. It works on Linux only.
type Netlink struct{}

// NewNetlink fails outside Linux.
func NewNetlink() (*Netlink, error) { return nil, errLinux }

// Close implements io.Closer.
func (*Netlink) Close() error { return nil }

// Devices implements Kernel.
func (*Netlink) Devices() ([]Device, error) { return nil, errLinux }

// Create implements Kernel.
func (*Netlink) Create(string) error { return errLinux }

// Delete implements Kernel.
func (*Netlink) Delete(string) error { return errLinux }

// Configure implements Kernel.
func (*Netlink) Configure(string, wgtypes.Config) error { return errLinux }

// AddAddress implements Kernel.
func (*Netlink) AddAddress(string, netip.Prefix) error { return errLinux }

// DeleteAddress implements Kernel.
func (*Netlink) DeleteAddress(string, netip.Prefix) error { return errLinux }

// AddRoute implements Kernel.
func (*Netlink) AddRoute(string, netip.Prefix) error { return errLinux }

// DeleteRoute implements Kernel.
func (*Netlink) DeleteRoute(string, netip.Prefix) error { return errLinux }

// Up implements Kernel.
func (*Netlink) Up(string) error { return errLinux }
