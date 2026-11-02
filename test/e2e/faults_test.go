//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Faults are nftables tables inside the node containers. Nothing changes on the host: docker networks,
// interfaces, addresses and routes stay as they are. Restoring a path deletes its table.
//
//	prerouting  priority raw - 10  drops on ingress, before conntrack, so established flows die too
//	postrouting priority 0         drops on egress, both locally generated and forwarded (pod) traffic
const (
	cutDirect = "strait-cut-direct"
	cutWAN    = "strait-cut-wan"
	cutSite   = "strait-cut-site"
)

// applyCut replaces one nftables table in the node.
func (e *env) applyCut(ctx context.Context, node, table, pre, post string) error {
	rules := fmt.Sprintf(`table inet %[1]s
delete table inet %[1]s
table inet %[1]s {
  chain pre  { type filter hook prerouting priority raw - 10; %[2]s }
  chain post { type filter hook postrouting priority 0; %[3]s }
}
`, table, pre, post)
	_, err := e.dockerStdin(ctx, 30*time.Second, strings.NewReader(rules), "exec", "-i", node, "nft", "-f", "-")
	return err
}

// removeCut deletes one nftables table from the node, if it exists.
func (e *env) removeCut(ctx context.Context, node, table string) error {
	out, err := e.docker(ctx, 30*time.Second, "exec", node, "nft", "list", "tables")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[1] == "inet" && f[2] == table {
			_, err = e.docker(ctx, 30*time.Second, "exec", node, "nft", "delete", "table", "inet", table)
			return err
		}
	}
	return nil
}

// directIface returns the node's interface on strait-direct.
func (e *env) directIface(ctx context.Context, s *site) (string, error) {
	out, err := e.docker(ctx, 30*time.Second, "exec", s.node, "ip", "-o", "-4", "addr", "show")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && strings.HasPrefix(f[3], s.directIP+"/") {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("node %s has no interface with %s", s.node, s.directIP)
}

// cutDirectLink drops everything on strait-direct at both sites.
func (e *env) cutDirectLink(ctx context.Context) error {
	for _, s := range e.sites() {
		i, err := e.directIface(ctx, s)
		if err != nil {
			return err
		}
		if err := e.applyCut(ctx, s.node, cutDirect, fmt.Sprintf("iifname %q drop;", i), fmt.Sprintf("oifname %q drop;", i)); err != nil {
			return err
		}
	}
	return nil
}

// cutWANPath drops strait-net traffic between the two sites' nodes. Each site keeps its API server and image pulls.
func (e *env) cutWANPath(ctx context.Context) error {
	for _, s := range e.sites() {
		w := e.peer(s).wanIP
		if err := e.applyCut(ctx, s.node, cutWAN, fmt.Sprintf("ip saddr %s drop;", w), fmt.Sprintf("ip daddr %s drop;", w)); err != nil {
			return err
		}
	}
	return nil
}

// restore deletes a cut at both sites.
func (e *env) restore(ctx context.Context, table string) error {
	for _, s := range e.sites() {
		if err := e.removeCut(ctx, s.node, table); err != nil {
			return err
		}
	}
	return nil
}

// siteDown isolates the site from the other site on both networks, then freezes its node. Pausing alone is not enough.
func (e *env) siteDown(ctx context.Context, s *site) error {
	i, err := e.directIface(ctx, s)
	if err != nil {
		return err
	}
	w := e.peer(s).wanIP
	if err := e.applyCut(ctx, s.node, cutSite,
		fmt.Sprintf("iifname %q drop; ip saddr %s drop;", i, w),
		fmt.Sprintf("oifname %q drop; ip daddr %s drop;", i, w)); err != nil {
		return err
	}
	_, err = e.docker(ctx, time.Minute, "pause", s.node)
	return err
}

// unpause unpauses every paused node of the site.
func (e *env) unpause(ctx context.Context, s *site) {
	nodes, err := e.nodes(ctx, s)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if st, err := e.docker(ctx, 30*time.Second, "inspect", "--format", "{{.State.Status}}", n); err == nil && strings.TrimSpace(st) == "paused" {
			_, _ = e.docker(ctx, time.Minute, "unpause", n)
		}
	}
}

// heal unpauses every node and removes every cut, so a failed run leaves clusters that can be read and deleted.
func (e *env) heal(ctx context.Context) {
	for _, s := range e.sites() {
		e.unpause(ctx, s)
		if s.node == "" {
			continue
		}
		for _, table := range []string{cutDirect, cutWAN, cutSite} {
			_ = e.removeCut(ctx, s.node, table)
		}
	}
}
