package validate

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/takodotid/strait/api/v1alpha1"
)

// Default sets the defaults of the kubebuilder default markers on obj, as the API server does.
// A zero value counts as unset: a typed object cannot tell an explicit zero from a missing field.
func Default(obj runtime.Object) {
	switch o := obj.(type) {
	case *v1alpha1.Site:
		defaultSite(&o.Spec)
	case *v1alpha1.SiteLink:
		defaultSiteLink(&o.Spec)
	case *v1alpha1.SiteService:
		defaultSiteService(&o.Spec)
	}
}

func defaultSite(s *v1alpha1.SiteSpec) {
	g := &s.Gateway
	setInt(&g.ControlPort, 30900)
	setRange(&g.ExportPorts, 30901, 31899)
	setRange(&g.ImportPorts, 15000, 15999)
	setInt(&g.PeerPort, 14999)
	setDuration(&s.Exchange.PollInterval, 2*time.Second)
	setDuration(&s.Exchange.PeerTimeout, 6*time.Second)
	if s.WireGuard != nil && s.WireGuard.PrivateKeySecretRef.Key == "" {
		s.WireGuard.PrivateKeySecretRef.Key = "privateKey"
	}
}

func defaultSiteLink(s *v1alpha1.SiteLinkSpec) {
	setInt(&s.Cost, 100)
	if w := s.WireGuard; w != nil {
		setInt(&w.ListenPort, 51820)
		if w.PersistentKeepalive == nil {
			w.PersistentKeepalive = &metav1.Duration{Duration: 25 * time.Second}
		}
	}
}

func defaultSiteService(s *v1alpha1.SiteServiceSpec) {
	if s.Policy.Multiple == "" {
		s.Policy.Multiple = v1alpha1.RouteNowhere
	}
	setDuration(&s.Policy.UnknownHold, 30*time.Second)
}

func setInt(v *int32, d int32) {
	if *v == 0 {
		*v = d
	}
}

func setRange(r *v1alpha1.PortRange, first, last int32) {
	if *r == (v1alpha1.PortRange{}) {
		*r = v1alpha1.PortRange{First: first, Last: last}
	}
}

func setDuration(v *metav1.Duration, d time.Duration) {
	if v.Duration == 0 {
		v.Duration = d
	}
}
