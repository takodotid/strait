// Package plugin holds the plugin contract of RFC#0003 and the endpoint helpers that plugins share.
package plugin

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

// Plugin observes the members of a SiteService that live at the local site.
// It reads through a client.Reader, which has no write methods.
type Plugin interface {
	// Observe returns the local site's declaration and one observation per local member.
	Observe(ctx context.Context, r client.Reader, svc *straitv1.SiteService, site string, now time.Time) observe.Report
}

// ReadOnly returns a reader that reads through r and has no other method, so no type assertion reaches the write
// methods of the client behind it. The client is in an unexported field, so reflection cannot hand it out either.
func ReadOnly(r client.Reader) client.Reader { return readOnly{r: r} }

type readOnly struct{ r client.Reader }

func (o readOnly) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return o.r.Get(ctx, key, obj, opts...)
}

func (o readOnly) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return o.r.List(ctx, list, opts...)
}

// Options are the plugins' settings from the controller's flags. The zero value turns every plugin on.
type Options struct {
	// CNPGOff turns the cnpg plugin off: it reads nothing and reports every member Unknown (RFC#0003, requirement 13).
	CNPGOff bool
}

// For returns the plugin that svc names.
func For(svc *straitv1.SiteService, o Options) (Plugin, error) {
	switch {
	case svc.Spec.Plugin.CNPG != nil:
		return CNPG{Off: o.CNPGOff}, nil
	case svc.Spec.Plugin.Declared != nil:
		return Declared{}, nil
	}
	return nil, fmt.Errorf("no plugin set")
}

// LocalMembers returns the members of svc at site.
func LocalMembers(svc *straitv1.SiteService, site string) []straitv1.Member {
	var out []straitv1.Member
	for _, m := range svc.Spec.Members {
		if m.Site == site {
			out = append(out, m)
		}
	}
	return out
}

// ErrNoService means the endpoint Service does not exist.
var ErrNoService = fmt.Errorf("service not found")

// ErrNoPort means the endpoint Service exists and has no port of the number asked for.
var ErrNoPort = fmt.Errorf("service port not found")

// noPort is the error of a Service that has no port of the number asked for. It is ErrNoPort.
type noPort struct {
	service string
	port    int32
}

func (e noPort) Error() string      { return fmt.Sprintf("service %s has no port %d", e.service, e.port) }
func (noPort) Is(target error) bool { return target == ErrNoPort }

// ReadyEndpoint returns the ready addresses of a Service and the target port of its port.
// It returns ErrNoService when the Service does not exist, and an error that is ErrNoPort when it has no such port.
func ReadyEndpoint(ctx context.Context, r client.Reader, namespace, name string, port int32) (observe.Endpoint, error) {
	ep := observe.Endpoint{Service: name, ServicePort: port}
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ep, ErrNoService
		}
		return ep, err
	}
	var sp *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port {
			sp = &svc.Spec.Ports[i]
		}
	}
	if sp == nil {
		return ep, noPort{service: name, port: port}
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: name}); err != nil {
		return ep, err
	}
	seen := map[string]bool{}
	for _, s := range slices.Items {
		var target int32
		for _, p := range s.Ports {
			if p.Port != nil && ptrString(p.Name) == sp.Name {
				target = *p.Port
			}
		}
		if target == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			for _, a := range e.Addresses {
				if !seen[a] {
					seen[a] = true
					ep.Addresses = append(ep.Addresses, a)
				}
			}
		}
		ep.Port = target
	}
	sort.Strings(ep.Addresses)
	return ep, nil
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
