// Package exchange serves this site's observations to peers and polls theirs, as RFC#0004 defines.
package exchange

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

const (
	// Kind is the kind of an observation document.
	Kind = "Observations"
	// Path is where a site serves its observation document.
	Path = "/v1alpha1/observations"
	// HealthPath answers 200 while the controller serves.
	HealthPath = "/healthz"
	// MaxDocumentSize is the largest document a reader accepts.
	MaxDocumentSize = 4 << 20
)

// APIVersion is the apiVersion of an observation document.
var APIVersion = v1alpha1.GroupVersion.String()

// errSiteMismatch rejects a document from a site other than the peer polled.
var errSiteMismatch = errors.New("site mismatch")

// Document is what one site publishes about its own members and exports.
type Document struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	Site        string    `json:"site"`
	GeneratedAt time.Time `json:"generatedAt"`
	Services    []Service `json:"services"`
	Exports     []Export  `json:"exports"`
}

// Service is one SiteService with a member at the publishing site.
type Service struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// DeclaredPrimary is the member the site declares primary. Empty when it declares none.
	DeclaredPrimary string   `json:"declaredPrimary"`
	Members         []Member `json:"members"`
}

// Member is the observation of one member at the publishing site.
type Member struct {
	Name       string         `json:"name"`
	Role       v1alpha1.Role  `json:"role"`
	Eligible   bool           `json:"eligible"`
	State      v1alpha1.State `json:"state"`
	Reason     string         `json:"reason"`
	ObservedAt time.Time      `json:"observedAt"`
	Addresses  []string       `json:"addresses"`
	Port       int32          `json:"port"`
}

// Export is one Export of the publishing site.
type Export struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Port is the gateway port that peers reach the export on.
	Port  int32 `json:"port"`
	Ready bool  `json:"ready"`
}

// MemberFrom returns the document entry of an observation.
func MemberFrom(o observe.Observation) Member {
	addresses := slices.Clone(o.Endpoint.Addresses)
	if addresses == nil {
		addresses = []string{}
	}
	slices.Sort(addresses)
	return Member{
		Name:       o.Member,
		Role:       o.Role,
		Eligible:   o.Eligible,
		State:      o.State,
		Reason:     o.Reason,
		ObservedAt: o.ObservedAt.UTC(),
		Addresses:  addresses,
		Port:       o.Endpoint.Port,
	}
}

// Observation returns the entry as an observation of a member at the given site.
func (m Member) Observation(site string) observe.Observation {
	return observe.Observation{
		Member:     m.Name,
		Site:       site,
		Role:       m.Role,
		Eligible:   m.Eligible,
		State:      m.State,
		Reason:     m.Reason,
		ObservedAt: m.ObservedAt,
		Endpoint:   observe.Endpoint{Addresses: slices.Clone(m.Addresses), Port: m.Port},
	}
}

// Service returns the document's entry for a SiteService.
func (d Document) Service(namespace, name string) (Service, bool) {
	for _, s := range d.Services {
		if s.Namespace == namespace && s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// validate checks a document polled from peer.
func (d Document) validate(peer string) error {
	if d.APIVersion != APIVersion || d.Kind != Kind {
		return fmt.Errorf("unexpected document %s %s, want %s %s", d.APIVersion, d.Kind, APIVersion, Kind)
	}
	if d.Site != peer {
		return fmt.Errorf("%w: document is from %q", errSiteMismatch, d.Site)
	}
	services := map[string]bool{}
	for _, s := range d.Services {
		key := s.Namespace + "/" + s.Name
		if services[key] {
			return fmt.Errorf("duplicate service %s", key)
		}
		services[key] = true
		members := map[string]bool{}
		for _, m := range s.Members {
			if members[m.Name] {
				return fmt.Errorf("duplicate member %s in service %s", m.Name, key)
			}
			members[m.Name] = true
		}
	}
	exports := map[string]bool{}
	for _, e := range d.Exports {
		key := e.Namespace + "/" + e.Name
		if exports[key] {
			return fmt.Errorf("duplicate export %s", key)
		}
		exports[key] = true
	}
	return nil
}
