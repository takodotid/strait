package gateway

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/links"
)

// PeerCluster starts the name of the Envoy cluster that reaches a peer's control port over each link ([RFC#0005]).
const PeerCluster = "peer/"

// ConditionHealthy is the SiteLink condition that holds the link's health.
const ConditionHealthy = "Healthy"

// Host is one endpoint of an Envoy cluster, as the admin interface reports it.
type Host struct {
	Address                 string
	Hostname                string
	Priority                int
	FailedActiveHealthCheck bool
	EDSHealth               string
}

// Healthy reports whether the host passes its active health check and EDS does not mark it unhealthy.
func (h Host) Healthy() bool { return !h.FailedActiveHealthCheck && h.EDSHealth != "UNHEALTHY" }

type clusterStatuses struct {
	ClusterStatuses []struct {
		Name         string `json:"name"`
		HostStatuses []struct {
			Address struct {
				SocketAddress struct {
					Address string `json:"address"`
				} `json:"socket_address"`
			} `json:"address"`
			Hostname     string `json:"hostname"`
			Priority     int    `json:"priority"`
			HealthStatus struct {
				FailedActiveHealthCheck bool   `json:"failed_active_health_check"`
				EDSHealthStatus         string `json:"eds_health_status"`
			} `json:"health_status"`
		} `json:"host_statuses"`
	} `json:"cluster_statuses"`
}

// ParseClusters reads the output of Envoy's admin /clusters?format=json into the hosts of each cluster.
func ParseClusters(b []byte) (map[string][]Host, error) {
	var cs clusterStatuses
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("parse Envoy clusters: %w", err)
	}
	out := map[string][]Host{}
	for _, c := range cs.ClusterStatuses {
		hosts := []Host{}
		for _, h := range c.HostStatuses {
			hosts = append(hosts, Host{
				Address:                 h.Address.SocketAddress.Address,
				Hostname:                h.Hostname,
				Priority:                h.Priority,
				FailedActiveHealthCheck: h.HealthStatus.FailedActiveHealthCheck,
				EDSHealth:               h.HealthStatus.EDSHealthStatus,
			})
		}
		out[c.Name] = hosts
	}
	return out, nil
}

// Observation is a link's health and whether it is the active link to its peer.
type Observation struct {
	Healthy bool
	Active  bool
	Reason  string
	Message string
}

// Observe decides the health of each link from Envoy's clusters, and picks the active link to each peer:
// the healthy link with the lowest rank. EnvoyErr is the error of reading Envoy's clusters.
// Held maps each wireguard link whose interface is in place to the interface; problems says why the others are not.
func Observe(ls []v1alpha1.SiteLink, clusters map[string][]Host, envoyErr error, held, problems map[string]string) map[string]Observation {
	out := map[string]Observation{}
	for peer, ranked := range links.ByPeer(ls) {
		active := false
		for _, l := range ranked {
			o := observe(l, clusters[PeerCluster+peer], envoyErr, held, problems)
			if o.Healthy && !active {
				o.Active, active = true, true
			}
			out[l.Name] = o
		}
	}
	return out
}

func observe(l v1alpha1.SiteLink, hosts []Host, envoyErr error, held, problems map[string]string) Observation {
	if envoyErr != nil {
		return Observation{Reason: "EnvoyUnreachable", Message: envoyErr.Error()}
	}
	if l.Spec.Transport == v1alpha1.TransportWireGuard && held[l.Name] == "" {
		return Observation{Reason: "NotConfigured", Message: cmp.Or(problems[l.Name], "the WireGuard interface is not in place")}
	}
	addr := links.Address(l)
	h, ok := hostAt(hosts, addr)
	switch {
	case !ok:
		return Observation{Reason: "NotInEnvoy", Message: fmt.Sprintf("Envoy cluster %s%s has no endpoint at %s", PeerCluster, l.Spec.Peer, addr)}
	case h.FailedActiveHealthCheck:
		return Observation{Reason: "HealthCheckFailed", Message: "the peer's control port does not answer GET /healthz over this link"}
	case !h.Healthy():
		return Observation{Reason: "EndpointUnhealthy", Message: "Envoy's endpoint discovery marks the endpoint unhealthy"}
	}
	o := Observation{Healthy: true, Reason: "HealthCheckPassed", Message: "the peer's control port answers GET /healthz over this link"}
	if p := problems[l.Name]; p != "" {
		o.Message += "; " + p
	}
	return o
}

// hostAt returns the host at a link's address. IP addresses compare by value, host names by text.
func hostAt(hosts []Host, addr string) (Host, bool) {
	want, isIP := HostAddr(addr)
	for _, h := range hosts {
		if isIP {
			if got, ok := HostAddr(h.Address); ok && got == want {
				return h, true
			}
			continue
		}
		if addr != "" && (strings.EqualFold(h.Hostname, addr) || h.Address == addr) {
			return h, true
		}
	}
	return Host{}, false
}

// LinkStatus returns a link's new status. Times are cut to whole seconds, as the API server stores them,
// so an unchanged observation gives an equal status.
func LinkStatus(l v1alpha1.SiteLink, o Observation, iface string, handshake, now time.Time) v1alpha1.SiteLinkStatus {
	st := *l.Status.DeepCopy()
	ts := metav1.NewTime(now.Truncate(time.Second))
	if st.Healthy != o.Healthy || st.LastTransitionTime == nil {
		st.LastTransitionTime = &ts
	}
	st.Healthy, st.Active, st.Interface = o.Healthy, o.Active, iface
	st.LastHandshakeTime = nil
	if iface != "" && !handshake.IsZero() {
		hs := metav1.NewTime(handshake.Truncate(time.Second))
		st.LastHandshakeTime = &hs
	}
	cond := metav1.Condition{Type: ConditionHealthy, Status: metav1.ConditionFalse, Reason: o.Reason, Message: o.Message, ObservedGeneration: l.Generation, LastTransitionTime: ts}
	if o.Healthy {
		cond.Status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, cond)
	return st
}
