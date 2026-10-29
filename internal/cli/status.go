package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

type siteView struct {
	Name               string `json:"name"`
	GatewayPod         string `json:"gatewayPod,omitempty"`
	GatewayAddress     string `json:"gatewayAddress,omitempty"`
	WireGuardPublicKey string `json:"wireGuardPublicKey,omitempty"`
}

type serviceView struct {
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	Mode      straitv1.Mode `json:"mode"`
	// Route are the routed members. Empty with a reason means nowhere.
	Route     []string     `json:"route"`
	Reason    string       `json:"reason,omitempty"`
	Since     *metav1.Time `json:"since,omitempty"`
	Endpoints int32        `json:"endpoints"`
}

type statusView struct {
	Sites        []siteView    `json:"sites"`
	Peers        []peerView    `json:"peers"`
	SiteServices []serviceView `json:"siteServices"`
}

func newStatus(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the site, its peers and where every application name points",
		Long: `status shows the state of one site in three parts.

The site: its name, the active gateway pod and its address, and the site's WireGuard public key.

The peers: one line each, with whether the peer's observations can be read and when they last were.

The application names: every SiteService in the namespace, or in every namespace with -A. ROUTE lists the members the name points at, or reads "nowhere" when the name has no endpoints. REASON says why. SINCE is how long ago the route last changed. A route of "-" means the controller has not written a status yet.`,
		Example: `  # The state of site a, with application names in the context's namespace.
  strait status --context site-a

  # The same, with application names in every namespace.
  strait status --context site-a -A`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ns, err := o.read()
			if err != nil {
				return err
			}
			v, err := readStatus(cmd, r, ns)
			if err != nil {
				return err
			}
			return o.write(cmd.OutOrStdout(), v, func(w io.Writer) { o.statusTable(w, v) })
		},
	}
}

func readStatus(cmd *cobra.Command, r client.Reader, ns string) (statusView, error) {
	ctx := cmd.Context()
	v := statusView{Sites: []siteView{}, SiteServices: []serviceView{}}

	var sites straitv1.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return v, fmt.Errorf("list Sites: %w", err)
	}
	for _, s := range sites.Items {
		v.Sites = append(v.Sites, siteView{
			Name:               s.Name,
			GatewayPod:         s.Status.Gateway.ActivePod,
			GatewayAddress:     s.Status.Gateway.Address,
			WireGuardPublicKey: s.Status.WireGuardPublicKey,
		})
	}

	peers, err := readPeers(cmd, r)
	if err != nil {
		return v, err
	}
	v.Peers = peers

	var services straitv1.SiteServiceList
	if err := r.List(ctx, &services, client.InNamespace(ns)); err != nil {
		return v, fmt.Errorf("list SiteServices: %w", err)
	}
	for _, s := range services.Items {
		v.SiteServices = append(v.SiteServices, serviceView{
			Namespace: s.Namespace,
			Name:      s.Name,
			Mode:      s.Spec.Mode,
			Route:     append([]string{}, s.Status.Route.Members...),
			Reason:    s.Status.Route.Reason,
			Since:     s.Status.Route.LastChangeTime,
			Endpoints: s.Status.Route.Endpoints,
		})
	}
	sort.Slice(v.SiteServices, func(i, j int) bool {
		a, b := v.SiteServices[i], v.SiteServices[j]
		return a.Namespace < b.Namespace || (a.Namespace == b.Namespace && a.Name < b.Name)
	})
	return v, nil
}

// route returns the routed members, nowhere, or - before the controller has written a route.
func route(members []string, reason string) string {
	switch {
	case len(members) > 0:
		return list(members)
	case reason != "":
		return "nowhere"
	}
	return "-"
}

func (o *options) statusTable(w io.Writer, v statusView) {
	if len(v.Sites) == 0 {
		fmt.Fprintln(w, "No Site found. Strait is not set up in this cluster.")
	} else {
		var rows [][]string
		for _, s := range v.Sites {
			rows = append(rows, []string{s.Name, dash(s.GatewayPod), dash(s.GatewayAddress), dash(s.WireGuardPublicKey)})
		}
		table(w, []string{"SITE", "GATEWAY", "ADDRESS", "WIREGUARD PUBLIC KEY"}, rows)
	}
	fmt.Fprintln(w)

	if len(v.Peers) == 0 {
		fmt.Fprintln(w, "No SitePeer found.")
	} else {
		o.peersTable(w, v.Peers)
	}
	fmt.Fprintln(w)

	if len(v.SiteServices) == 0 {
		fmt.Fprintln(w, "No SiteService found.")
		return
	}
	var rows [][]string
	for _, s := range v.SiteServices {
		rows = append(rows, []string{s.Namespace, s.Name, string(s.Mode), route(s.Route, s.Reason), dash(s.Reason), o.age(s.Since)})
	}
	table(w, []string{"NAMESPACE", "NAME", "MODE", "ROUTE", "REASON", "SINCE"}, rows)
}
