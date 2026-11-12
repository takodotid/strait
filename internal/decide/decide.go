// Package decide picks a SiteService's route from observations, as RFC#0002 defines.
package decide

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

// SiteView is what is known about one site.
type SiteView struct {
	// Known is true for the local site, and for a peer whose document is fresh.
	Known bool
	// Declared is the member the site declares primary. Empty when it declares none.
	Declared string
	// Conflicting holds the members that the site's workloads declare primary, when they name more than one. Only the
	// local site's view holds it: the site declares none to its peers then.
	Conflicting []string
}

// Input holds every input of a decision.
type Input struct {
	Spec      straitv1.SiteServiceSpec
	LocalSite string
	// Sites holds every known site. A site that is missing is not known.
	Sites map[string]SiteView
	// Observations holds observations by member name. A member that is missing is Unknown.
	Observations map[string]observe.Observation
	// Previous is the previous route's members.
	Previous []string
	// LastConfirmed is the last time each member was Stable and eligible.
	LastConfirmed map[string]time.Time
	Now           time.Time
	// Started is when the controller started to poll peers. The startup hold counts from it.
	Started time.Time
	// Read holds the peers whose document was read at least once since Started.
	Read map[string]bool
	// Peers is the number of peers that the controller polls: the local site's SitePeers, or none while polling is
	// off.
	Peers int
	// LastPeerRead is the last time a document of any peer was read. Zero when none was. The isolated-site policy
	// counts from it, or from Started if that is later.
	LastPeerRead time.Time
}

// Result is a route and the facts that the status reports.
type Result struct {
	// Members are the routed members, in spec order. Empty means nowhere.
	Members []string
	Reason  string
	Message string
	// Multiple is true when two or more members are writable. Single mode only.
	Multiple bool
	// Disagree is true when two known sites declare different primaries, or the workloads of one site do. Single mode only.
	Disagree bool
	// Held are the routed members kept while Unknown.
	Held []string
	// Unmatched holds each declaration of a known site that is no member's name, such as "site a declares pg-east",
	// in site order. It changes nothing else of the result. Single mode only.
	Unmatched []string
}

// Decide returns the route for the input.
func Decide(in Input) Result {
	if in.Spec.Mode == straitv1.ModeSet {
		return decideSet(in)
	}
	return decideSingle(in)
}

// Observation returns the member's observation as the decision counts it. A member whose site is not known is
// Unknown with reason SiteUnknown, whatever was last observed (RFC#0002, requirement 16). A member of a known site
// with no observation is Unknown with reason NotObserved.
func (in Input) Observation(m straitv1.Member) observe.Observation {
	if s, ok := in.Sites[m.Site]; !ok || !s.Known {
		return observe.Unknown(m.Name, m.Site, "SiteUnknown", in.Now)
	}
	if o, ok := in.Observations[m.Name]; ok {
		return o
	}
	return observe.Unknown(m.Name, m.Site, "NotObserved", in.Now)
}

// withinHold reports whether an Unknown member may stay in the route.
func (in Input) withinHold(member string) bool {
	last, ok := in.LastConfirmed[member]
	return ok && in.Now.Sub(last) < in.Spec.Policy.UnknownHold.Duration
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// declarations returns the distinct primaries declared by known sites, and by the workloads of a site that name more
// than one, sorted.
func (in Input) declarations() []string {
	seen := map[string]bool{}
	for _, s := range in.Sites {
		if s.Known && s.Declared != "" {
			seen[s.Declared] = true
		}
		for _, d := range s.Conflicting {
			if s.Known {
				seen[d] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// unmatched returns each declaration of a known site, or of its workloads, that is no member's name, in site order.
func (in Input) unmatched() []string {
	names := map[string]bool{}
	for _, m := range in.Spec.Members {
		names[m.Name] = true
	}
	var out []string
	for _, site := range slices.Sorted(maps.Keys(in.Sites)) {
		s := in.Sites[site]
		if !s.Known {
			continue
		}
		if s.Declared != "" && !names[s.Declared] {
			out = append(out, fmt.Sprintf("site %s declares %s", site, s.Declared))
		}
		for _, d := range s.Conflicting {
			if !names[d] {
				out = append(out, fmt.Sprintf("the workloads of site %s declare %s", site, d))
			}
		}
	}
	return out
}

// unanimous returns the primary that every site with a member declares, or "".
func (in Input) unanimous() string {
	declared := ""
	for _, m := range in.Spec.Members {
		s, ok := in.Sites[m.Site]
		if !ok || !s.Known || s.Declared == "" {
			return ""
		}
		if declared != "" && s.Declared != declared {
			return ""
		}
		declared = s.Declared
	}
	return declared
}

// Unread returns the sites that host a member, other than the local site, and have not been read since the controller
// began to poll peers, sorted.
func (in Input) Unread() []string {
	var out []string
	for _, m := range in.Spec.Members {
		if m.Site != in.LocalSite && !in.Read[m.Site] && !contains(out, m.Site) {
			out = append(out, m.Site)
		}
	}
	sort.Strings(out)
	return out
}

// awaiting returns the sites with a member that have not been read since the controller started, sorted, while the
// startup hold lasts. Empty when the hold does not apply.
func (in Input) awaiting() []string {
	hold := in.Spec.Policy.StartupHold.Duration
	if hold <= 0 || in.Now.Sub(in.Started) >= hold {
		return nil
	}
	return in.Unread()
}

// Isolated reports whether the local site counts as isolated (RFC#0002, requirement 45), and for how long no peer
// has been read: isolationTimeout is set, a member is at another site, the controller polls a peer, no peer is known,
// and no peer was read for isolationTimeout, counted from Started at the earliest.
func (in Input) Isolated() (time.Duration, bool) {
	starts := in.IsolationStarts()
	if starts.IsZero() {
		return 0, false
	}
	for name, s := range in.Sites {
		if name != in.LocalSite && s.Known {
			return 0, false
		}
	}
	return in.Now.Sub(starts) + in.Spec.Policy.IsolationTimeout.Duration, !in.Now.Before(starts)
}

// IsolationStarts returns when the local site counts as isolated if no peer is read before then. Zero when the policy
// is off or does not apply.
func (in Input) IsolationStarts() time.Time {
	timeout := in.Spec.Policy.IsolationTimeout.Duration
	if timeout <= 0 || in.Peers == 0 || !in.remoteMember() {
		return time.Time{}
	}
	since := in.Started
	if in.LastPeerRead.After(since) {
		since = in.LastPeerRead
	}
	return since.Add(timeout)
}

// remoteMember reports whether a member is at a site other than the local one.
func (in Input) remoteMember() bool {
	for _, m := range in.Spec.Members {
		if m.Site != in.LocalSite {
			return true
		}
	}
	return false
}

// decideSingle applies the isolated-site policy and the startup hold to the route that the table picks: it never
// routes to a local member while the site is isolated or the hold lasts. Isolation comes first (RFC#0002,
// requirement 47).
func decideSingle(in Input) Result {
	r := singleTable(in)
	if len(r.Members) != 1 || !in.local(r.Members[0]) {
		return r
	}
	if unseen, isolated := in.Isolated(); isolated {
		r.Message = fmt.Sprintf("%s would be routed (%s), but it is local and no peer has been read for %s; isolationTimeout %s",
			r.Members[0], r.Reason, unseen.Round(time.Second), in.Spec.Policy.IsolationTimeout.Duration)
		r.Reason = straitv1.ReasonSiteIsolated
		r.Members, r.Held = nil, nil
		return r
	}
	if sites := in.awaiting(); len(sites) > 0 {
		r.Message = fmt.Sprintf("%s would be routed (%s), but it is local and site %s has not been read since the controller started; startupHold %s",
			r.Members[0], r.Reason, strings.Join(sites, ", "), in.Spec.Policy.StartupHold.Duration)
		r.Reason = straitv1.ReasonAwaitingPeers
		r.Members, r.Held = nil, nil
	}
	return r
}

// local reports whether the member is at the local site.
func (in Input) local(member string) bool {
	for _, m := range in.Spec.Members {
		if m.Name == member {
			return m.Site == in.LocalSite
		}
	}
	return false
}

// singleTable picks the route of Single mode by the table of RFC#0002, requirement 18.
func singleTable(in Input) Result {
	var writable []string
	transitioning := false
	for _, m := range in.Spec.Members {
		o := in.Observation(m)
		if o.Writable() {
			writable = append(writable, m.Name)
		}
		if o.State == straitv1.StateTransitioning {
			transitioning = true
		}
	}
	decl := in.declarations()
	r := Result{Multiple: len(writable) >= 2, Disagree: len(decl) >= 2, Unmatched: in.unmatched()}

	// Row 1.
	if r.Disagree {
		r.Reason = straitv1.ReasonDeclarationsDisagree
		r.Message = fmt.Sprintf("sites declare different primaries: %s", strings.Join(decl, ", "))
		for _, name := range slices.Sorted(maps.Keys(in.Sites)) {
			if s := in.Sites[name]; s.Known && len(s.Conflicting) > 0 {
				r.Message += fmt.Sprintf("; the workloads of site %s declare %s", name, strings.Join(s.Conflicting, ", "))
			}
		}
		return r
	}
	// Rows 2 and 3.
	if r.Multiple {
		if in.Spec.Policy.Multiple == straitv1.PreferDeclaredIfConsistent {
			if d := in.unanimous(); d != "" && contains(writable, d) {
				r.Members = []string{d}
				r.Reason = straitv1.ReasonPreferDeclared
				r.Message = fmt.Sprintf("writable members %s; every site declares %s", strings.Join(writable, ", "), d)
				return r
			}
		}
		r.Reason = straitv1.ReasonMultipleWritable
		r.Message = fmt.Sprintf("writable members: %s", strings.Join(writable, ", "))
		return r
	}
	// Rows 4 and 5.
	if len(writable) == 1 {
		w := writable[0]
		if len(decl) == 1 && decl[0] != w {
			r.Reason = straitv1.ReasonNotDeclared
			r.Message = fmt.Sprintf("%s is writable but sites declare %s", w, decl[0])
			return r
		}
		r.Members = []string{w}
		r.Reason = straitv1.ReasonActive
		r.Message = fmt.Sprintf("%s is the only writable member", w)
		return r
	}
	// Row 6.
	if transitioning {
		r.Reason = straitv1.ReasonTransitioning
		r.Message = "no member is confirmed writable and a member is moving"
		return r
	}
	// Rows 7 and 8.
	for _, m := range in.Spec.Members {
		if !contains(in.Previous, m.Name) || in.Observation(m).State != straitv1.StateUnknown {
			continue
		}
		if in.withinHold(m.Name) {
			r.Members = []string{m.Name}
			r.Held = []string{m.Name}
			r.Reason = straitv1.ReasonHeldUnknown
			r.Message = fmt.Sprintf("%s is Unknown; held for %s after it was last confirmed", m.Name, in.Spec.Policy.UnknownHold.Duration)
			return r
		}
		r.Reason = straitv1.ReasonUnknownTimeout
		r.Message = fmt.Sprintf("%s is Unknown for longer than %s", m.Name, in.Spec.Policy.UnknownHold.Duration)
		return r
	}
	// Row 9.
	for _, m := range in.Spec.Members {
		if in.Observation(m).State == straitv1.StateUnknown {
			r.Reason = straitv1.ReasonMemberUnknown
			r.Message = fmt.Sprintf("no member is writable and %s is Unknown", m.Name)
			return r
		}
	}
	// Row 10.
	r.Reason = straitv1.ReasonNoEligibleMember
	r.Message = "no member is writable"
	return r
}

func decideSet(in Input) Result {
	type candidate struct {
		name      string
		local     bool
		held      bool
		confirmed bool
	}
	var candidates []candidate
	expired := false
	for _, m := range in.Spec.Members {
		o := in.Observation(m)
		c := candidate{name: m.Name, local: m.Site == in.LocalSite}
		switch {
		case o.Confirmed():
			c.confirmed = true
		case contains(in.Previous, m.Name) && o.State == straitv1.StateTransitioning:
		case contains(in.Previous, m.Name) && o.State == straitv1.StateUnknown:
			if !in.withinHold(m.Name) {
				expired = true
				continue
			}
			c.held = true
		default:
			continue
		}
		candidates = append(candidates, c)
	}

	locality := in.Spec.Locality
	if locality == "" {
		locality = straitv1.LocalFirst
	}
	// LocalFirst prefers the local site only while it has a confirmed member.
	hasLocal := false
	for _, c := range candidates {
		hasLocal = hasLocal || (c.local && c.confirmed)
	}
	var r Result
	for _, c := range candidates {
		if c.local || locality == straitv1.Any || (locality == straitv1.LocalFirst && !hasLocal) {
			r.Members = append(r.Members, c.name)
			if c.held {
				r.Held = append(r.Held, c.name)
			}
		}
	}

	switch {
	case len(r.Members) == 0 && expired:
		r.Reason = straitv1.ReasonUnknownTimeout
		r.Message = fmt.Sprintf("routed members are Unknown for longer than %s", in.Spec.Policy.UnknownHold.Duration)
	case len(r.Members) == 0:
		r.Reason = straitv1.ReasonNoEligibleMember
		r.Message = fmt.Sprintf("no eligible member for locality %s", locality)
	case len(r.Held) == len(r.Members):
		r.Reason = straitv1.ReasonHeldUnknown
		r.Message = fmt.Sprintf("every routed member is Unknown; held for %s", in.Spec.Policy.UnknownHold.Duration)
	default:
		r.Reason = straitv1.ReasonEligible
		r.Message = fmt.Sprintf("eligible members for locality %s: %s", locality, strings.Join(r.Members, ", "))
	}
	return r
}
