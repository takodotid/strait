// Package decide picks a SiteService's route from observations, as RFC#0002 defines.
package decide

import (
	"fmt"
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
}

// Result is a route and the facts that the status reports.
type Result struct {
	// Members are the routed members, in spec order. Empty means nowhere.
	Members []string
	Reason  string
	Message string
	// Multiple is true when two or more members are writable. Single mode only.
	Multiple bool
	// Disagree is true when two known sites declare different primaries. Single mode only.
	Disagree bool
	// Held are the routed members kept while Unknown.
	Held []string
}

// Decide returns the route for the input.
func Decide(in Input) Result {
	if in.Spec.Mode == straitv1.ModeSet {
		return decideSet(in)
	}
	return decideSingle(in)
}

// observation returns the member's observation. A member whose site is not known is Unknown, whatever was last observed.
func (in Input) observation(m straitv1.Member) observe.Observation {
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

// declarations returns the distinct primaries declared by known sites, sorted.
func (in Input) declarations() []string {
	seen := map[string]bool{}
	for _, s := range in.Sites {
		if s.Known && s.Declared != "" {
			seen[s.Declared] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
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

func decideSingle(in Input) Result {
	var writable []string
	transitioning := false
	for _, m := range in.Spec.Members {
		o := in.observation(m)
		if o.Writable() {
			writable = append(writable, m.Name)
		}
		if o.State == straitv1.StateTransitioning {
			transitioning = true
		}
	}
	decl := in.declarations()
	r := Result{Multiple: len(writable) >= 2, Disagree: len(decl) >= 2}

	// Row 1.
	if r.Disagree {
		r.Reason = straitv1.ReasonDeclarationsDisagree
		r.Message = fmt.Sprintf("sites declare different primaries: %s", strings.Join(decl, ", "))
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
		if !contains(in.Previous, m.Name) || in.observation(m).State != straitv1.StateUnknown {
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
		if in.observation(m).State == straitv1.StateUnknown {
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
		o := in.observation(m)
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
