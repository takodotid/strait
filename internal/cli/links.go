package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	straitv1 "github.com/takodotid/strait/api/v1"
)

type linkView struct {
	Name       string              `json:"name"`
	Peer       string              `json:"peer"`
	Transport  straitv1.Transport  `json:"transport"`
	Encryption straitv1.Encryption `json:"encryption"`
	Cost       int32               `json:"cost"`
	// EndpointDirect is true on a direct link with endpointDirect.
	EndpointDirect bool `json:"endpointDirect"`
	Healthy        bool `json:"healthy"`
	// Active is true while this link carries the pair's traffic.
	Active             bool         `json:"active"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Interface          string       `json:"interface,omitempty"`
	LastHandshakeTime  *metav1.Time `json:"lastHandshakeTime,omitempty"`
}

func newLinks(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "links",
		Short: "Show every link to a peer, its health, and which link carries the traffic",
		Long: `links shows every SiteLink: one network path between this site and a peer.

Links are grouped by peer, in the order traffic prefers them: lowest COST first, then lowest name. ACTIVE is true for the one link that carries the pair's traffic now: the healthy link that comes first. HEALTHY is true while the peer's control port answers over the link. CHANGED is how long ago HEALTHY last changed.

ENCRYPTION is None on a direct link, which carries traffic in clear on a private path, and WireGuard on a wireguard link.

ENDPOINT-DIRECT is true on a direct link with endpointDirect. While that link is ACTIVE, application names point at the remote member's own pods, with no gateway hop. strait explain shows it as PATH EndpointDirect.

SiteLinks are cluster-scoped, so -n and -A do not apply.`,
		Example: `  strait links --context site-a
  strait links --context site-a -o yaml`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := o.read()
			if err != nil {
				return err
			}
			var links straitv1.SiteLinkList
			if err := r.List(cmd.Context(), &links); err != nil {
				return fmt.Errorf("list SiteLinks: %w", err)
			}
			v := []linkView{}
			for _, l := range links.Items {
				enc := l.Spec.Encryption
				if enc == "" && l.Spec.Transport == straitv1.TransportWireGuard {
					enc = straitv1.EncryptionWireGuard
				}
				v = append(v, linkView{
					Name:               l.Name,
					Peer:               l.Spec.Peer,
					Transport:          l.Spec.Transport,
					Encryption:         enc,
					Cost:               l.Spec.Cost,
					EndpointDirect:     l.Spec.Direct != nil && l.Spec.Direct.EndpointDirect,
					Healthy:            l.Status.Healthy,
					Active:             l.Status.Active,
					LastTransitionTime: l.Status.LastTransitionTime,
					Interface:          l.Status.Interface,
					LastHandshakeTime:  l.Status.LastHandshakeTime,
				})
			}
			sort.Slice(v, func(i, j int) bool {
				a, b := v[i], v[j]
				if a.Peer != b.Peer {
					return a.Peer < b.Peer
				}
				if a.Cost != b.Cost {
					return a.Cost < b.Cost
				}
				return a.Name < b.Name
			})
			return o.write(cmd.OutOrStdout(), map[string]any{"links": v}, func(w io.Writer) {
				if len(v) == 0 {
					fmt.Fprintln(w, "No SiteLink found.")
					return
				}
				var rows [][]string
				for _, l := range v {
					rows = append(rows, []string{l.Peer, l.Name, string(l.Transport), dash(string(l.Encryption)), number(l.Cost),
						boolean(l.EndpointDirect), boolean(l.Healthy), boolean(l.Active), o.age(l.LastTransitionTime)})
				}
				table(w, []string{"PEER", "LINK", "TRANSPORT", "ENCRYPTION", "COST", "ENDPOINT-DIRECT", "HEALTHY", "ACTIVE", "CHANGED"}, rows)
			})
		},
	}
}
