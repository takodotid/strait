package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

type routeView struct {
	// Members are the routed members. Empty with a reason means nowhere.
	Members        []string     `json:"members"`
	Reason         string       `json:"reason,omitempty"`
	Meaning        string       `json:"meaning,omitempty"`
	Message        string       `json:"message,omitempty"`
	LastChangeTime *metav1.Time `json:"lastChangeTime,omitempty"`
	Endpoints      int32        `json:"endpoints"`
}

type memberView struct {
	straitv1.MemberStatus
	// Observed is false when the status holds nothing about a member of the spec.
	Observed bool `json:"observed"`
	Routed   bool `json:"routed"`
}

type explainView struct {
	Namespace          string                 `json:"namespace"`
	Name               string                 `json:"name"`
	Generation         int64                  `json:"generation"`
	ObservedGeneration int64                  `json:"observedGeneration"`
	Mode               straitv1.Mode          `json:"mode"`
	Locality           straitv1.Locality      `json:"locality,omitempty"`
	Plugin             straitv1.PluginSpec    `json:"plugin"`
	Policy             straitv1.Policy        `json:"policy"`
	Route              routeView              `json:"route"`
	Members            []memberView           `json:"members"`
	Declarations       []straitv1.Declaration `json:"declarations"`
	Conditions         []metav1.Condition     `json:"conditions"`
}

func newExplain(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "explain SITESERVICE",
		Short: "Explain why one application name points where it does",
		Long: `explain shows one SiteService as its controller last saw it: the route, its reason and what the reason means, every member's observation, every site's declared primary, the policy, and the conditions.

explain reads the SiteService's status. It never decides the route itself, so it shows the controller's view and not a second opinion.

In the members table, ROUTED is true for the members the name points at. PATH is how a routed member with endpoints takes traffic: Local is its own pods at this site, Gateway is the local gateway over the active link, EndpointDirect is its own pods at its site over a direct link, with no gateway. A member with STATE - is in the spec but not in the status yet. OBSERVED is the age of the member's last observation. CONFIRMED is how long ago it was last seen Stable and eligible.

With -A, explain looks for the name in every namespace. It fails if more than one namespace holds it.`,
		Example: `  # Explain the application name pg in namespace app at site a.
  strait explain pg -n app --context site-a

  # The same, as JSON.
  strait explain pg -n app --context site-a -o json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ns, err := o.read()
			if err != nil {
				return err
			}
			ss, err := getSiteService(cmd, r, ns, args[0])
			if err != nil {
				return err
			}
			v := explain(ss)
			return o.write(cmd.OutOrStdout(), v, func(w io.Writer) { o.explainTable(w, v) })
		},
	}
}

func getSiteService(cmd *cobra.Command, r client.Reader, ns, name string) (*straitv1.SiteService, error) {
	if ns != "" {
		var ss straitv1.SiteService
		err := r.Get(cmd.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ss)
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("SiteService %q not found in namespace %q", name, ns)
		}
		return &ss, err
	}
	var all straitv1.SiteServiceList
	if err := r.List(cmd.Context(), &all); err != nil {
		return nil, fmt.Errorf("list SiteServices: %w", err)
	}
	var found []straitv1.SiteService
	var namespaces []string
	for _, ss := range all.Items {
		if ss.Name == name {
			found = append(found, ss)
			namespaces = append(namespaces, ss.Namespace)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("SiteService %q not found in any namespace", name)
	case 1:
		return &found[0], nil
	}
	slices.Sort(namespaces)
	return nil, usageError{fmt.Errorf("SiteService %q is in namespaces %s: pick one with -n", name, strings.Join(namespaces, ", "))}
}

func explain(ss *straitv1.SiteService) explainView {
	st := ss.Status
	v := explainView{
		Namespace:          ss.Namespace,
		Name:               ss.Name,
		Generation:         ss.Generation,
		ObservedGeneration: st.ObservedGeneration,
		Mode:               ss.Spec.Mode,
		Locality:           ss.Spec.Locality,
		Plugin:             ss.Spec.Plugin,
		Policy:             ss.Spec.Policy,
		Route: routeView{
			Members:        append([]string{}, st.Route.Members...),
			Reason:         st.Route.Reason,
			Meaning:        reasons[st.Route.Reason],
			Message:        st.Route.Message,
			LastChangeTime: st.Route.LastChangeTime,
			Endpoints:      st.Route.Endpoints,
		},
		Members:      []memberView{},
		Declarations: append([]straitv1.Declaration{}, st.Declarations...),
		Conditions:   append([]metav1.Condition{}, st.Conditions...),
	}
	// Members of the spec first, in spec order, then any the status still holds.
	statuses := map[string]straitv1.MemberStatus{}
	for _, m := range st.Members {
		statuses[m.Name] = m
	}
	for _, m := range ss.Spec.Members {
		s, ok := statuses[m.Name]
		if !ok {
			s = straitv1.MemberStatus{Name: m.Name, Site: m.Site}
		}
		delete(statuses, m.Name)
		v.Members = append(v.Members, memberView{MemberStatus: s, Observed: ok, Routed: slices.Contains(st.Route.Members, m.Name)})
	}
	for _, m := range st.Members {
		if _, ok := statuses[m.Name]; ok {
			v.Members = append(v.Members, memberView{MemberStatus: m, Observed: true, Routed: slices.Contains(st.Route.Members, m.Name)})
		}
	}
	return v
}

func pluginText(p straitv1.PluginSpec) string {
	switch {
	case p.CNPG != nil:
		return "cnpg, role " + string(p.CNPG.Role)
	case p.Declared != nil && p.Declared.Active != "":
		return "declared, active " + p.Declared.Active
	case p.Declared != nil:
		return "declared"
	}
	return "-"
}

func (o *options) explainTable(w io.Writer, v explainView) {
	pairs := [][2]string{
		{"SiteService", v.Namespace + "/" + v.Name},
		{"Mode", string(v.Mode)},
	}
	if v.Mode == straitv1.ModeSet {
		locality := v.Locality
		if locality == "" {
			locality = straitv1.LocalFirst
		}
		pairs = append(pairs, [2]string{"Locality", string(locality)})
	}
	pairs = append(pairs,
		[2]string{"Plugin", pluginText(v.Plugin)},
		[2]string{"Policy", fmt.Sprintf("multiple %s, unknownHold %s", dash(string(v.Policy.Multiple)), v.Policy.UnknownHold.Duration)},
	)
	switch {
	case v.ObservedGeneration == 0 && v.Route.Reason == "":
		pairs = append(pairs, [2]string{"Status", "none. The controller has not written a status yet."})
	case v.ObservedGeneration < v.Generation:
		pairs = append(pairs, [2]string{"Status", fmt.Sprintf("stale. It is from generation %d, and the spec is at generation %d.", v.ObservedGeneration, v.Generation)})
	}
	r := v.Route
	routeText := route(r.Members, r.Reason)
	if routeText == "nowhere" {
		routeText = "nowhere. The name has no endpoints, and new connections are refused."
	}
	pairs = append(pairs,
		[2]string{"Route", routeText},
		[2]string{"Reason", dash(r.Reason)},
	)
	if r.Meaning != "" {
		pairs = append(pairs, [2]string{"Meaning", r.Meaning})
	}
	pairs = append(pairs,
		[2]string{"Message", dash(r.Message)},
		[2]string{"Since", o.when(r.LastChangeTime)},
		[2]string{"Endpoints", number(r.Endpoints)},
	)
	fields(w, pairs...)

	fmt.Fprintln(w, "\nMembers:")
	var rows [][]string
	for _, m := range v.Members {
		eligible := "-"
		if m.Observed {
			eligible = boolean(m.Eligible)
		}
		rows = append(rows, []string{m.Name, m.Site, boolean(m.Routed), dash(string(m.Path)), dash(string(m.Role)), eligible, dash(string(m.State)),
			dash(m.Reason), o.age(m.ObservedTime), o.age(m.LastConfirmedTime)})
	}
	table(w, []string{"NAME", "SITE", "ROUTED", "PATH", "ROLE", "ELIGIBLE", "STATE", "REASON", "OBSERVED", "CONFIRMED"}, rows)

	fmt.Fprintln(w, "\nDeclarations:")
	if len(v.Declarations) == 0 {
		fmt.Fprintln(w, "None.")
	} else {
		rows = nil
		for _, d := range v.Declarations {
			rows = append(rows, []string{d.Site, dash(d.Primary)})
		}
		table(w, []string{"SITE", "PRIMARY"}, rows)
	}

	fmt.Fprintln(w, "\nConditions:")
	if len(v.Conditions) == 0 {
		fmt.Fprintln(w, "None.")
		return
	}
	rows = nil
	for _, c := range v.Conditions {
		rows = append(rows, []string{c.Type, string(c.Status), dash(c.Reason), o.age(&c.LastTransitionTime), dash(c.Message)})
	}
	table(w, []string{"TYPE", "STATUS", "REASON", "SINCE", "MESSAGE"}, rows)
}
