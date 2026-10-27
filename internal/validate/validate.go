// Package validate checks Strait objects with the rules of their CRD schemas, and sets of objects with the rules that span them.
package validate

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/takodotid/strait/api/v1alpha1"
)

// Severity is how serious a problem is.
type Severity string

const (
	// SeverityError means the object would be rejected or cannot work.
	SeverityError Severity = "Error"
	// SeverityWarning means a rule could not be checked with the objects at hand.
	SeverityWarning Severity = "Warning"
)

// Ref names an object.
type Ref struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// String returns kind/name, or kind/namespace/name for a namespaced object.
func (r Ref) String() string {
	if r.Namespace != "" {
		return r.Kind + "/" + r.Namespace + "/" + r.Name
	}
	return r.Kind + "/" + r.Name
}

// RefOf returns the reference of obj.
func RefOf(obj runtime.Object) Ref {
	r := Ref{Kind: Kind(obj)}
	if m, err := meta.Accessor(obj); err == nil {
		r.Namespace = m.GetNamespace()
		r.Name = m.GetName()
	}
	return r
}

// Kind returns the kind of a Strait object from its Go type. Typed objects read from a cluster often carry no TypeMeta.
func Kind(obj runtime.Object) string {
	switch obj.(type) {
	case *v1alpha1.Site:
		return "Site"
	case *v1alpha1.SitePeer:
		return "SitePeer"
	case *v1alpha1.SiteLink:
		return "SiteLink"
	case *v1alpha1.Export:
		return "Export"
	case *v1alpha1.Import:
		return "Import"
	case *v1alpha1.SiteService:
		return "SiteService"
	}
	return obj.GetObjectKind().GroupVersionKind().Kind
}

// Problem is one finding about one object.
type Problem struct {
	// Index is the position of the object in the slice given to Set.
	Index    int      `json:"-"`
	Object   Ref      `json:"object"`
	Severity Severity `json:"severity"`
	// Field is the path of the field, such as spec.wireguard.listenPort.
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// String returns the problem on one line.
func (p Problem) String() string {
	if p.Field == "" {
		return fmt.Sprintf("%s %s: %s", p.Severity, p.Object, p.Message)
	}
	return fmt.Sprintf("%s %s: %s: %s", p.Severity, p.Object, p.Field, p.Message)
}

func detail(e *field.Error, d string) *field.Error {
	e.Detail = d
	return e
}

func newProblem(i int, obj runtime.Object, s Severity, e *field.Error) Problem {
	return Problem{Index: i, Object: RefOf(obj), Severity: s, Field: e.Field, Message: e.ErrorBody()}
}

// Object checks one object with the rules of its CRD schema, on a defaulted copy, as the API server does.
// It does not change obj and does not check status.
func Object(obj runtime.Object) field.ErrorList {
	switch o := obj.(type) {
	case *v1alpha1.Site:
		return Site(o)
	case *v1alpha1.SitePeer:
		return SitePeer(o)
	case *v1alpha1.SiteLink:
		return SiteLink(o)
	case *v1alpha1.Export:
		return Export(o)
	case *v1alpha1.Import:
		return Import(o)
	case *v1alpha1.SiteService:
		return SiteService(o)
	}
	return field.ErrorList{field.NotSupported(field.NewPath("kind"), Kind(obj), kinds)}
}

var kinds = []string{"Site", "SitePeer", "SiteLink", "Export", "Import", "SiteService"}

// Set checks every object with Object, then the rules that span objects:
//   - at most one Site;
//   - a SiteLink's peer has a SitePeer, a warning when the set holds none;
//   - a wireguard SiteLink needs spec.wireguard on the Site and on its SitePeer, a warning when the set holds no Site;
//   - two wireguard SiteLinks to one peer use different listen ports.
//
// Problems are ordered by object.
func Set(objs []runtime.Object) []Problem {
	var out []Problem
	defaulted := make([]runtime.Object, len(objs))
	for i, obj := range objs {
		for _, e := range Object(obj) {
			out = append(out, newProblem(i, obj, SeverityError, e))
		}
		defaulted[i] = obj.DeepCopyObject()
		Default(defaulted[i])
	}
	out = append(out, spanning(defaulted)...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

func spanning(objs []runtime.Object) []Problem {
	var out []Problem
	var site *v1alpha1.Site
	peers := map[string]*v1alpha1.SitePeer{}
	for i, obj := range objs {
		switch o := obj.(type) {
		case *v1alpha1.Site:
			if site != nil {
				out = append(out, newProblem(i, o, SeverityError, field.Forbidden(field.NewPath("metadata", "name"),
					fmt.Sprintf("a cluster holds exactly one Site, and Site/%s comes earlier in the set", site.Name))))
				continue
			}
			site = o
		case *v1alpha1.SitePeer:
			peers[o.Name] = o
		}
	}

	type key struct {
		peer string
		port int32
	}
	ports := map[key]string{}
	for i, obj := range objs {
		l, ok := obj.(*v1alpha1.SiteLink)
		if !ok {
			continue
		}
		spec := field.NewPath("spec")
		peer, found := peers[l.Spec.Peer]
		if !found && l.Spec.Peer != "" {
			out = append(out, newProblem(i, l, SeverityWarning, detail(field.NotFound(spec.Child("peer"), l.Spec.Peer),
				"no SitePeer of that name in the set")))
		}
		if l.Spec.Transport != v1alpha1.TransportWireGuard {
			continue
		}
		transport := spec.Child("transport")
		switch {
		case site == nil:
			out = append(out, newProblem(i, l, SeverityWarning, field.Invalid(transport, l.Spec.Transport,
				"no Site in the set: cannot check that it sets spec.wireguard")))
		case site.Spec.WireGuard == nil:
			out = append(out, newProblem(i, l, SeverityError, field.Invalid(transport, l.Spec.Transport,
				fmt.Sprintf("the wireguard transport needs spec.wireguard on Site/%s", site.Name))))
		}
		if found && peer.Spec.WireGuard == nil {
			out = append(out, newProblem(i, l, SeverityError, field.Invalid(transport, l.Spec.Transport,
				fmt.Sprintf("the wireguard transport needs spec.wireguard on SitePeer/%s", peer.Name))))
		}
		if l.Spec.WireGuard == nil {
			continue
		}
		k := key{l.Spec.Peer, l.Spec.WireGuard.ListenPort}
		if other, ok := ports[k]; ok {
			out = append(out, newProblem(i, l, SeverityError, detail(field.Duplicate(spec.Child("wireguard", "listenPort"), k.port),
				fmt.Sprintf("SiteLink/%s to the same peer uses this listen port", other))))
			continue
		}
		ports[k] = l.Name
	}
	return out
}
