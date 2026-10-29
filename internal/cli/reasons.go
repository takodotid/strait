package cli

import (
	"sort"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// reasons gives the meaning of each route reason. explain and the skill document both use it.
var reasons = map[string]string{
	straitv1.ReasonActive:               "Single mode. Exactly one member is writable, and no known site declares another. The name points at it.",
	straitv1.ReasonPreferDeclared:       "Single mode. Two or more members are writable. The policy PreferDeclaredIfConsistent is set, every site with a member declares the same primary, and that member is writable. The name points at it.",
	straitv1.ReasonMultipleWritable:     "Single mode. Two or more members are writable. Routed nowhere.",
	straitv1.ReasonDeclarationsDisagree: "Single mode. Two known sites declare different primaries. Routed nowhere. No policy relaxes this.",
	straitv1.ReasonNotDeclared:          "Single mode. Exactly one member is writable, but a known site declares another member. Routed nowhere.",
	straitv1.ReasonTransitioning:        "Single mode. No member is confirmed writable and a member is moving, for example during a switchover. Routed nowhere.",
	straitv1.ReasonHeldUnknown:          "The routed members are Unknown, but each was confirmed less than policy.unknownHold ago. The previous route is kept.",
	straitv1.ReasonUnknownTimeout:       "A member of the previous route has been Unknown for longer than policy.unknownHold, and no other member can be routed. Routed nowhere.",
	straitv1.ReasonMemberUnknown:        "No member is writable and a member cannot be read. It may be writable. Routed nowhere.",
	straitv1.ReasonNoEligibleMember:     "No member is writable in Single mode, or eligible for the locality in Set mode. Routed nowhere.",
	straitv1.ReasonEligible:             "Set mode. The name points at every Stable and eligible member that the locality allows. Members of the previous route that are Transitioning, or Unknown within policy.unknownHold, stay.",
}

type reason struct {
	Name    string
	Meaning string
}

// sortedReasons returns the reasons in name order.
func sortedReasons() []reason {
	out := make([]reason, 0, len(reasons))
	for name, meaning := range reasons {
		out = append(out, reason{name, meaning})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
