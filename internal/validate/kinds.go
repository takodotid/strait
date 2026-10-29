package validate

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// The patterns of the kubebuilder Pattern markers.
var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	wireGuardKey = regexp.MustCompile(`^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$`)
)

// Site checks a Site.
func Site(s *straitv1.Site) field.ErrorList {
	// The CRD defaults a gateway Service's type only when the whole Service is absent,
	// so the rules on the fields that depend on the type read the type as written.
	written := s.Spec.Gateway
	s = s.DeepCopy()
	defaultSite(&s.Spec)
	errs := objectMeta(&s.ObjectMeta)
	spec := field.NewPath("spec")

	g := s.Spec.Gateway
	gp := spec.Child("gateway")
	errs = append(errs, port(gp.Child("controlPort"), g.ControlPort)...)
	errs = append(errs, portRange(gp.Child("exportPorts"), g.ExportPorts)...)
	errs = append(errs, portRange(gp.Child("importPorts"), g.ImportPorts)...)
	errs = append(errs, port(gp.Child("peerPort"), g.PeerPort)...)
	hasExport, hasImport := g.ExportPorts != straitv1.PortRange{}, g.ImportPorts != straitv1.PortRange{}
	errs = append(errs, rule(gp,
		!hasExport || g.ControlPort == 0 || g.ExportPorts.First > g.ControlPort || g.ExportPorts.Last < g.ControlPort,
		"controlPort must be outside exportPorts")...)
	errs = append(errs, rule(gp,
		!hasImport || !hasExport || g.ImportPorts.First > g.ExportPorts.Last || g.ImportPorts.Last < g.ExportPorts.First,
		"importPorts must not overlap exportPorts")...)
	errs = append(errs, rule(gp,
		!hasImport || g.PeerPort == 0 || g.ImportPorts.First > g.PeerPort || g.ImportPorts.Last < g.PeerPort,
		"peerPort must be outside importPorts")...)

	for _, e := range []struct {
		name    string
		written string
		straitv1.ServiceExposure
	}{{"directService", written.DirectService.Type, g.DirectService}, {"wireGuardService", written.WireGuardService.Type, g.WireGuardService}} {
		p := gp.Child(e.name)
		if e.Type != "ClusterIP" && e.Type != "NodePort" && e.Type != "LoadBalancer" {
			errs = append(errs, field.NotSupported(p.Child("type"), e.Type, []string{"ClusterIP", "NodePort", "LoadBalancer"}))
		}
		errs = append(errs, enum(p.Child("externalTrafficPolicy"), e.ExternalTrafficPolicy, false, "Cluster", "Local")...)
		rp := p.Child("loadBalancerSourceRanges")
		if n := len(e.LoadBalancerSourceRanges); n > 64 {
			errs = append(errs, field.TooMany(rp, n, 64))
		}
		for i, r := range e.LoadBalancerSourceRanges {
			errs = append(errs, cidr(rp.Index(i), r)...)
		}
		errs = append(errs, rule(p,
			e.LoadBalancerSourceRanges == nil || e.written == "LoadBalancer",
			"loadBalancerSourceRanges needs type LoadBalancer")...)
		errs = append(errs, rule(p,
			e.AllocateLoadBalancerNodePorts == nil || e.written == "LoadBalancer",
			"allocateLoadBalancerNodePorts needs type LoadBalancer")...)
		errs = append(errs, rule(p,
			e.ExternalTrafficPolicy == "" || (e.written != "" && e.written != "ClusterIP"),
			"externalTrafficPolicy needs type NodePort or LoadBalancer")...)
	}

	x := s.Spec.Exchange
	errs = append(errs, rule(spec.Child("exchange"),
		x.PeerTimeout.Duration == 0 || x.PollInterval.Duration == 0 || x.PeerTimeout.Duration >= 2*x.PollInterval.Duration,
		"peerTimeout must be at least twice pollInterval")...)

	if w := s.Spec.WireGuard; w != nil {
		errs = append(errs, required(spec.Child("wireguard", "privateKeySecretRef", "name"), w.PrivateKeySecretRef.Name, 0)...)
	}
	return errs
}

// SitePeer checks a SitePeer.
func SitePeer(s *straitv1.SitePeer) field.ErrorList {
	s = s.DeepCopy()
	Default(s)
	errs := objectMeta(&s.ObjectMeta)
	errs = append(errs, port(field.NewPath("spec", "controlPort"), s.Spec.ControlPort)...)
	if w := s.Spec.WireGuard; w != nil {
		p := field.NewPath("spec", "wireguard", "publicKey")
		if w.PublicKey == "" {
			errs = append(errs, field.Required(p, ""))
		} else if !wireGuardKey.MatchString(w.PublicKey) {
			errs = append(errs, field.Invalid(p, w.PublicKey, "must be a base64 WireGuard key, matching "+wireGuardKey.String()))
		}
	}
	return errs
}

// SiteLink checks a SiteLink.
func SiteLink(l *straitv1.SiteLink) field.ErrorList {
	l = l.DeepCopy()
	defaultSiteLink(&l.Spec)
	errs := objectMeta(&l.ObjectMeta)
	s := l.Spec
	spec := field.NewPath("spec")

	errs = append(errs, required(spec.Child("peer"), s.Peer, 63)...)
	errs = append(errs, enum(spec.Child("transport"), s.Transport, true, straitv1.TransportDirect, straitv1.TransportWireGuard)...)
	errs = append(errs, between(spec.Child("cost"), s.Cost, 0, 65535)...)
	errs = append(errs, enum(spec.Child("encryption"), s.Encryption, false, straitv1.EncryptionNone, straitv1.EncryptionWireGuard)...)
	if d := s.Direct; d != nil {
		errs = append(errs, ip(spec.Child("direct", "address"), d.Address)...)
	}
	if w := s.WireGuard; w != nil {
		wp := spec.Child("wireguard")
		errs = append(errs, port(wp.Child("listenPort"), w.ListenPort)...)
		errs = append(errs, cidr(wp.Child("address"), w.Address)...)
		errs = append(errs, ip(wp.Child("peerAddress"), w.PeerAddress)...)
	}

	direct := s.Transport == straitv1.TransportDirect
	errs = append(errs, rule(spec,
		(direct && s.Direct != nil && s.WireGuard == nil) || (!direct && s.WireGuard != nil && s.Direct == nil),
		"set direct for the direct transport, wireguard for the wireguard transport, and not the other")...)
	errs = append(errs, rule(spec,
		!direct || s.Encryption == straitv1.EncryptionNone,
		"a direct link carries traffic in clear: declare encryption: None")...)
	errs = append(errs, rule(spec,
		s.Transport != straitv1.TransportWireGuard || s.Encryption == "" || s.Encryption == straitv1.EncryptionWireGuard,
		"a wireguard link is encrypted by WireGuard")...)
	return errs
}

// Export checks an Export.
func Export(e *straitv1.Export) field.ErrorList {
	errs := objectMeta(&e.ObjectMeta)
	return append(errs, servicePortRef(field.NewPath("spec", "service"), e.Spec.Service)...)
}

// Import checks an Import.
func Import(i *straitv1.Import) field.ErrorList {
	errs := objectMeta(&i.ObjectMeta)
	spec := field.NewPath("spec")
	errs = append(errs, required(spec.Child("site"), i.Spec.Site, 63)...)
	return append(errs, required(spec.Child("export"), i.Spec.Export, 253)...)
}

// SiteService checks a SiteService.
func SiteService(ss *straitv1.SiteService) field.ErrorList {
	ss = ss.DeepCopy()
	defaultSiteService(&ss.Spec)
	errs := objectMeta(&ss.ObjectMeta)
	errs = append(errs, rule(nil,
		ss.Name == "" || (dns1035.MatchString(ss.Name) && len(ss.Name) <= 63),
		"the name must be a DNS-1035 label, since it names a Service")...)
	s := ss.Spec
	spec := field.NewPath("spec")

	errs = append(errs, enum(spec.Child("mode"), s.Mode, true, straitv1.ModeSingle, straitv1.ModeSet)...)
	errs = append(errs, enum(spec.Child("locality"), s.Locality, false, straitv1.LocalFirst, straitv1.LocalOnly, straitv1.Any)...)

	pp := spec.Child("port")
	errs = append(errs, label(pp.Child("name"), s.Port.Name, 15)...)
	errs = append(errs, port(pp.Child("port"), s.Port.Port)...)

	pl := spec.Child("plugin")
	if c := s.Plugin.CNPG; c != nil {
		errs = append(errs, enum(pl.Child("cnpg", "role"), c.Role, true, straitv1.CNPGRolePrimary, straitv1.CNPGRoleReplica, straitv1.CNPGRoleAny)...)
	}
	errs = append(errs, rule(pl, (s.Plugin.CNPG != nil) != (s.Plugin.Declared != nil), "set exactly one plugin")...)

	mp := spec.Child("members")
	switch n := len(s.Members); {
	case n == 0:
		errs = append(errs, field.Required(mp, "must have at least 1 item"))
	case n > 16:
		errs = append(errs, field.TooMany(mp, n, 16))
	}
	seen := map[string]bool{}
	for i, m := range s.Members {
		p := mp.Index(i)
		errs = append(errs, label(p.Child("name"), m.Name, 63)...)
		errs = append(errs, label(p.Child("site"), m.Site, 63)...)
		if c := m.CNPG; c != nil {
			errs = append(errs, required(p.Child("cnpg", "cluster"), c.Cluster, 50)...)
		}
		if m.Service != nil {
			errs = append(errs, servicePortRef(p.Child("service"), *m.Service)...)
		}
		// The list is a map keyed by name.
		if seen[m.Name] {
			errs = append(errs, field.Duplicate(p, map[string]string{"name": m.Name}))
		}
		seen[m.Name] = true
	}

	pol := spec.Child("policy")
	errs = append(errs, enum(pol.Child("multiple"), s.Policy.Multiple, false, straitv1.RouteNowhere, straitv1.PreferDeclaredIfConsistent)...)
	errs = append(errs, rule(pol,
		s.Policy.UnknownHold.Duration >= 0 && s.Policy.UnknownHold.Duration <= 10*time.Minute,
		"unknownHold must be between 0s and 10m")...)
	errs = append(errs, rule(pol,
		s.Policy.StartupHold.Duration >= 0 && s.Policy.StartupHold.Duration <= 10*time.Minute,
		"startupHold must be between 0s and 10m")...)

	cnpg, declared := s.Plugin.CNPG, s.Plugin.Declared
	errs = append(errs, rule(spec,
		s.Mode == straitv1.ModeSet || s.Locality == "",
		"locality applies to Set mode only")...)
	errs = append(errs, rule(spec,
		s.Mode == straitv1.ModeSingle || s.Policy.StartupHold.Duration == 0,
		"startupHold applies to Single mode only")...)
	errs = append(errs, rule(spec,
		cnpg == nil || (cnpg.Role == straitv1.CNPGRolePrimary) == (s.Mode == straitv1.ModeSingle),
		"cnpg role primary requires Single mode; replica and any require Set mode")...)
	errs = append(errs, rule(spec,
		declared == nil || s.Mode == straitv1.ModeSet || declared.Active != "",
		"the declared plugin needs active in Single mode")...)
	errs = append(errs, rule(spec,
		declared == nil || declared.Active == "" || slices.ContainsFunc(s.Members, func(m straitv1.Member) bool { return m.Name == declared.Active }),
		"declared.active must name a member")...)
	errs = append(errs, rule(spec,
		!slices.ContainsFunc(s.Members, func(m straitv1.Member) bool {
			if cnpg != nil {
				return m.CNPG == nil || m.Service != nil
			}
			return m.Service == nil || m.CNPG != nil
		}),
		"every member must carry the source of the plugin in use, and no other")...)
	errs = append(errs, rule(spec,
		len(seen) == len(s.Members),
		"member names must be unique")...)
	return errs
}

func objectMeta(m *metav1.ObjectMeta) field.ErrorList {
	var errs field.ErrorList
	p := field.NewPath("metadata")
	switch {
	case m.Name == "" && m.GenerateName == "":
		errs = append(errs, field.Required(p.Child("name"), "name or generateName is required"))
	case m.Name != "":
		for _, msg := range apivalidation.NameIsDNSSubdomain(m.Name, false) {
			errs = append(errs, field.Invalid(p.Child("name"), m.Name, msg))
		}
	}
	if m.Namespace != "" {
		for _, msg := range apivalidation.ValidateNamespaceName(m.Namespace, false) {
			errs = append(errs, field.Invalid(p.Child("namespace"), m.Namespace, msg))
		}
	}
	return errs
}

func servicePortRef(p *field.Path, r straitv1.ServicePortRef) field.ErrorList {
	errs := required(p.Child("name"), r.Name, 63)
	return append(errs, port(p.Child("port"), r.Port)...)
}

// rule reports a failed XValidation rule at the struct that carries it, as the API server does.
// ip checks a required IP address, as the CRD's MaxLength and isIP rule do.
func ip(p *field.Path, v string) field.ErrorList {
	if errs := required(p, v, 45); errs != nil {
		return errs
	}
	if _, err := netip.ParseAddr(v); err != nil {
		return field.ErrorList{field.Invalid(p, v, "must be an IP address")}
	}
	return nil
}

// cidr checks a required address in CIDR notation, as the CRD's MaxLength and isCIDR rule do.
func cidr(p *field.Path, v string) field.ErrorList {
	if errs := required(p, v, 49); errs != nil {
		return errs
	}
	if _, err := netip.ParsePrefix(v); err != nil {
		return field.ErrorList{field.Invalid(p, v, "must be an address in CIDR notation")}
	}
	return nil
}

var dns1035 = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

func rule(p *field.Path, ok bool, message string) field.ErrorList {
	if ok {
		return nil
	}
	return field.ErrorList{field.Invalid(p, "object", message)}
}

func between(p *field.Path, v, lo, hi int32) field.ErrorList {
	if v < lo || v > hi {
		return field.ErrorList{field.Invalid(p, v, fmt.Sprintf("must be between %d and %d", lo, hi))}
	}
	return nil
}

// port checks a required port number.
func port(p *field.Path, v int32) field.ErrorList {
	if v == 0 {
		return field.ErrorList{field.Required(p, "")}
	}
	return between(p, v, 1, 65535)
}

func portRange(p *field.Path, r straitv1.PortRange) field.ErrorList {
	errs := port(p.Child("first"), r.First)
	errs = append(errs, port(p.Child("last"), r.Last)...)
	return append(errs, rule(p, r.First <= r.Last, "first must not exceed last")...)
}

// required checks a required string of at least one character and, when max is not 0, at most max characters.
func required(p *field.Path, v string, max int) field.ErrorList {
	if v == "" {
		return field.ErrorList{field.Required(p, "")}
	}
	if max > 0 && utf8.RuneCountInString(v) > max {
		return field.ErrorList{field.TooLongCharacters(p, v, max)}
	}
	return nil
}

// label checks a required DNS label of at most max characters.
func label(p *field.Path, v string, max int) field.ErrorList {
	errs := required(p, v, max)
	if v != "" && !dnsLabel.MatchString(v) {
		errs = append(errs, field.Invalid(p, v, "must match "+dnsLabel.String()))
	}
	return errs
}

func enum[T ~string](p *field.Path, v T, isRequired bool, values ...T) field.ErrorList {
	switch {
	case v == "" && isRequired:
		return field.ErrorList{field.Required(p, "")}
	case v == "" || slices.Contains(values, v):
		return nil
	}
	return field.ErrorList{field.NotSupported(p, v, values)}
}
