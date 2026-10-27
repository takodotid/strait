package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
)

type peerView struct {
	Name         string       `json:"name"`
	Reachable    bool         `json:"reachable"`
	LastSeenTime *metav1.Time `json:"lastSeenTime,omitempty"`
	LastError    string       `json:"lastError,omitempty"`
	Services     int32        `json:"services"`
	Exports      int32        `json:"exports"`
}

func newPeers(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "Show every peer site and whether its observations can be read",
		Long: `peers shows every SitePeer: one peer site that this site polls for observations.

REACHABLE is true while the peer's last observation document is fresh. While it is false, every member at that peer counts as Unknown. LAST SEEN is how long ago the last poll succeeded. SERVICES and EXPORTS count what the peer's last document held. LAST ERROR is the error of the last failed poll.

SitePeers are cluster-scoped, so -n and -A do not apply.`,
		Example: `  strait peers --context site-a
  strait peers --context site-a -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := o.read()
			if err != nil {
				return err
			}
			peers, err := readPeers(cmd, r)
			if err != nil {
				return err
			}
			return o.write(cmd.OutOrStdout(), map[string]any{"peers": peers}, func(w io.Writer) {
				if len(peers) == 0 {
					fmt.Fprintln(w, "No SitePeer found.")
					return
				}
				o.peersTable(w, peers)
			})
		},
	}
}

func readPeers(cmd *cobra.Command, r client.Reader) ([]peerView, error) {
	var peers v1alpha1.SitePeerList
	if err := r.List(cmd.Context(), &peers); err != nil {
		return nil, fmt.Errorf("list SitePeers: %w", err)
	}
	out := []peerView{}
	for _, p := range peers.Items {
		out = append(out, peerView{
			Name:         p.Name,
			Reachable:    p.Status.Reachable,
			LastSeenTime: p.Status.LastSeenTime,
			LastError:    p.Status.LastError,
			Services:     p.Status.Services,
			Exports:      p.Status.Exports,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (o *options) peersTable(w io.Writer, peers []peerView) {
	var rows [][]string
	for _, p := range peers {
		seen := o.age(p.LastSeenTime)
		if p.LastSeenTime == nil {
			seen = "never"
		}
		rows = append(rows, []string{p.Name, boolean(p.Reachable), seen, number(p.Services), number(p.Exports), dash(p.LastError)})
	}
	table(w, []string{"PEER", "REACHABLE", "LAST SEEN", "SERVICES", "EXPORTS", "LAST ERROR"}, rows)
}
