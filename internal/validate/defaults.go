package validate

import (
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Fields holds the paths of the fields that an object's source sets to a value other than null, such as
// "spec.gateway.lease.duration". The defaults leave such a field as written, also when it holds a zero value, as the
// API server does. Nil Fields count every zero value as unset: a typed object cannot tell an explicit zero from a
// missing field.
type Fields map[string]bool

// FieldsOf returns the fields that a JSON object sets. It looks into objects, not into lists.
func FieldsOf(data []byte) (Fields, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	f := Fields{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if v == nil {
				continue
			}
			f[prefix+k] = true
			if c, ok := v.(map[string]any); ok {
				walk(prefix+k+".", c)
			}
		}
	}
	walk("", m)
	return f, nil
}

// Default sets the defaults of the kubebuilder default markers on obj, as the API server does.
// A zero value counts as unset: a typed object cannot tell an explicit zero from a missing field.
func Default(obj runtime.Object) { defaults(obj, nil) }

// defaults is Default, which leaves alone the fields that f holds.
func defaults(obj runtime.Object, f Fields) {
	switch o := obj.(type) {
	case *straitv1.Site:
		defaultSite(&o.Spec, f)
	case *straitv1.SitePeer:
		setInt(f, "spec.controlPort", &o.Spec.ControlPort, 30900)
	case *straitv1.SiteLink:
		defaultSiteLink(&o.Spec, f)
	case *straitv1.SiteService:
		defaultSiteService(&o.Spec, f)
	}
}

func defaultSite(s *straitv1.SiteSpec, f Fields) {
	g := &s.Gateway
	setInt(f, "spec.gateway.controlPort", &g.ControlPort, 30900)
	setRange(f, "spec.gateway.exportPorts", &g.ExportPorts, 30901, 31899)
	setRange(f, "spec.gateway.importPorts", &g.ImportPorts, 15000, 15999)
	setInt(f, "spec.gateway.peerPort", &g.PeerPort, 14999)
	// The CRD defaults a gateway Service's type only when the whole Service is absent.
	if g.DirectService.Type == "" && !f["spec.gateway.directService"] {
		g.DirectService.Type = "ClusterIP"
	}
	if g.WireGuardService.Type == "" && !f["spec.gateway.wireGuardService"] {
		g.WireGuardService.Type = "NodePort"
	}
	setDuration(f, "spec.gateway.lease.duration", &g.Lease.Duration, 6*time.Second)
	setDuration(f, "spec.gateway.lease.renewDeadline", &g.Lease.RenewDeadline, 4*time.Second)
	setDuration(f, "spec.gateway.lease.retryPeriod", &g.Lease.RetryPeriod, time.Second)
	setDuration(f, "spec.exchange.pollInterval", &s.Exchange.PollInterval, 2*time.Second)
	setDuration(f, "spec.exchange.peerTimeout", &s.Exchange.PeerTimeout, 6*time.Second)
	if s.WireGuard != nil && s.WireGuard.PrivateKeySecretRef.Key == "" && !f["spec.wireguard.privateKeySecretRef.key"] {
		s.WireGuard.PrivateKeySecretRef.Key = "privateKey"
	}
}

func defaultSiteLink(s *straitv1.SiteLinkSpec, f Fields) {
	setInt(f, "spec.cost", &s.Cost, 100)
	if w := s.WireGuard; w != nil {
		setInt(f, "spec.wireguard.listenPort", &w.ListenPort, 31820)
		if w.PersistentKeepalive == nil {
			w.PersistentKeepalive = &metav1.Duration{Duration: 25 * time.Second}
		}
	}
}

func defaultSiteService(s *straitv1.SiteServiceSpec, f Fields) {
	if s.Policy.Multiple == "" && !f["spec.policy.multiple"] {
		s.Policy.Multiple = straitv1.RouteNowhere
	}
	setDuration(f, "spec.policy.unknownHold", &s.Policy.UnknownHold, 30*time.Second)
}

func setInt(f Fields, path string, v *int32, d int32) {
	if *v == 0 && !f[path] {
		*v = d
	}
}

func setRange(f Fields, path string, r *straitv1.PortRange, first, last int32) {
	if *r == (straitv1.PortRange{}) && !f[path] {
		*r = straitv1.PortRange{First: first, Last: last}
	}
}

func setDuration(f Fields, path string, v *metav1.Duration, d time.Duration) {
	if v.Duration == 0 && !f[path] {
		v.Duration = d
	}
}
