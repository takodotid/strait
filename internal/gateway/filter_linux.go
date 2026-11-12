//go:build linux

package gateway

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// The nftables table of the gateway pod's network namespace that filters what arrives on the WireGuard interfaces.
// Its input chain admits what is addressed to a tunnel address. Its forward chain drops all of it: a pod's namespace
// can forward, since IPv4 forwarding starts as the node's. Only the gateway agent may change the namespace's netfilter
// state: Envoy has no capability.
var filterTable = &nftables.Table{Name: "strait", Family: nftables.TableFamilyINet}

// filterSet is the set of the tunnel addresses of one family, and where that family's packets hold the destination.
type filterSet struct {
	name     string
	keyType  nftables.SetDatatype
	proto    byte
	offset   uint32
	addrLen  uint32
	contains func(netip.Addr) bool
}

var filterSets = []filterSet{
	{"tunnel4", nftables.TypeIPAddr, unix.NFPROTO_IPV4, 16, 4, netip.Addr.Is4},
	{"tunnel6", nftables.TypeIP6Addr, unix.NFPROTO_IPV6, 24, 16, netip.Addr.Is6},
}

// Filter implements Kernel. It reads the sets of the table; the table's rules are written with them, in one change.
func (n *Netlink) Filter() ([]netip.Addr, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, err := nftables.New()
	if err != nil {
		return nil, false, err
	}
	tables, err := c.ListTablesOfFamily(filterTable.Family)
	if err != nil {
		return nil, false, fmt.Errorf("list nftables tables: %w", err)
	}
	if !slices.ContainsFunc(tables, func(t *nftables.Table) bool { return t.Name == filterTable.Name }) {
		return nil, false, nil
	}
	var out []netip.Addr
	for _, fs := range filterSets {
		s, err := c.GetSetByName(filterTable, fs.name)
		if err != nil {
			// A table without its sets is not the agent's: it is replaced.
			return nil, false, nil
		}
		els, err := c.GetSetElements(s)
		if err != nil {
			return nil, false, fmt.Errorf("list the elements of set %s: %w", fs.name, err)
		}
		for _, e := range els {
			if a, ok := netip.AddrFromSlice(e.Key); ok {
				out = append(out, a)
			}
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out, true, nil
}

// SetFilter implements Kernel. One transaction creates the table if it is absent, deletes it, and writes it again, so
// that no packet meets a filter that is partly written.
func (n *Netlink) SetFilter(addrs []netip.Addr) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, err := nftables.New()
	if err != nil {
		return err
	}
	c.AddTable(filterTable)
	c.DelTable(filterTable)
	t := c.AddTable(filterTable)
	accept := nftables.ChainPolicyAccept
	ch := c.AddChain(&nftables.Chain{
		Name: "wireguard-input", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &accept,
	})
	fwd := c.AddChain(&nftables.Chain{
		Name: "wireguard-forward", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept,
	})
	// iifname "st*" drop
	c.AddRule(&nftables.Rule{Table: t, Chain: fwd, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte(Prefix)},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}})
	for _, fs := range filterSets {
		s := &nftables.Set{Table: t, Name: fs.name, KeyType: fs.keyType}
		var els []nftables.SetElement
		for _, a := range addrs {
			if a = a.Unmap(); fs.contains(a) {
				els = append(els, nftables.SetElement{Key: a.AsSlice()})
			}
		}
		if err := c.AddSet(s, els); err != nil {
			return fmt.Errorf("set %s: %w", fs.name, err)
		}
		// iifname "st*" meta nfproto <family> <family> daddr != @<set> drop
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte(Prefix)},
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{fs.proto}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: fs.offset, Len: fs.addrLen},
			&expr.Lookup{SourceRegister: 1, SetName: s.Name, SetID: s.ID, Invert: true},
			&expr.Verdict{Kind: expr.VerdictDrop},
		}})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("write nftables table %s: %w", filterTable.Name, err)
	}
	return nil
}
