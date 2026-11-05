package exchange

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/takodotid/strait/internal/observe"
)

type key struct {
	namespace string
	name      string
}

type localService struct {
	declared string
	members  []observe.Observation
}

// Local holds what this site publishes. It is safe for concurrent use.
type Local struct {
	mu       sync.Mutex
	services map[key]localService
	exports  map[key]Export
}

// SetService sets a SiteService's declaration and its local members' observations.
// Document lists only the observations whose Site is the local site, so Site must be set.
func (l *Local) SetService(namespace, name string, declared string, members []observe.Observation) {
	copied := make([]observe.Observation, len(members))
	for i, o := range members {
		o.Endpoint.Addresses = slices.Clone(o.Endpoint.Addresses)
		copied[i] = o
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.services == nil {
		l.services = map[key]localService{}
	}
	l.services[key{namespace, name}] = localService{declared: declared, members: copied}
}

// DeleteService removes a SiteService.
func (l *Local) DeleteService(namespace, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.services, key{namespace, name})
}

// SetExport sets an Export's gateway port and readiness.
func (l *Local) SetExport(namespace, name string, port int32, ready bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exports == nil {
		l.exports = map[key]Export{}
	}
	l.exports[key{namespace, name}] = Export{Namespace: namespace, Name: name, Port: port, Ready: ready}
}

// DeleteExport removes an Export.
func (l *Local) DeleteExport(namespace, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.exports, key{namespace, name})
}

// ReasonObservationStale is the reason of a member that the publishing site last observed longer ago than the age
// limit when the document was generated.
const ReasonObservationStale = "ObservationStale"

// MinAgeLimit is the least age limit of an observation. A member is observed again at each decision, at least every
// 2s, and the decisions of every SiteService share one worker. A shorter limit would age the members of a healthy site.
const MinAgeLimit = 6 * time.Second

// Document returns the document of the site, sorted by namespace and name.
// A SiteService with no member at the site is left out.
// A member whose last observation is older than maxAge at now, or than MinAgeLimit if that is more, is published as
// Unknown, with no address, so that a reconcile that hangs does not keep an old observation fresh at the peers
// (RFC#0004). Its declaration stays as last observed. A maxAge of 0 ages nothing. now must come from time.Now, and so
// must the observation times, so that the age is measured on the monotonic clock.
func (l *Local) Document(site string, now time.Time, maxAge time.Duration) Document {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxAge > 0 {
		maxAge = max(maxAge, MinAgeLimit)
	}
	d := Document{
		APIVersion:  APIVersion,
		Kind:        Kind,
		Site:        site,
		GeneratedAt: now.UTC(),
		Services:    []Service{},
		Exports:     []Export{},
	}
	for k, s := range l.services {
		svc := Service{Namespace: k.namespace, Name: k.name, DeclaredPrimary: s.declared, Members: []Member{}}
		for _, o := range s.members {
			if o.Site != site {
				continue
			}
			if maxAge > 0 && now.Sub(o.ObservedAt) > maxAge {
				o = observe.Unknown(o.Member, o.Site, ReasonObservationStale, o.ObservedAt)
			}
			svc.Members = append(svc.Members, MemberFrom(o))
		}
		if len(svc.Members) == 0 {
			continue
		}
		slices.SortFunc(svc.Members, func(a, b Member) int { return cmp.Compare(a.Name, b.Name) })
		d.Services = append(d.Services, svc)
	}
	for _, e := range l.exports {
		d.Exports = append(d.Exports, e)
	}
	slices.SortFunc(d.Services, func(a, b Service) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(d.Exports, func(a, b Export) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return d
}
