package plugin

import (
	"context"
	"errors"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

var (
	cnpgCluster = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	cnpgPooler  = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}
)

// transitioningPhases are the Cluster phases in which the primary is moving or restarting.
var transitioningPhases = []string{
	"Switchover in progress",
	"Failing over",
	"Promoting to primary cluster",
	"Setting up primary",
	"Primary instance is being restarted in-place",
	"Primary instance is being restarted without a switchover",
}

// postgresPort is the port of every CloudNativePG Service and Pooler Service.
const postgresPort = 5432

// CNPG observes CloudNativePG clusters. It reads only the fields that RFC#0003 names.
type CNPG struct{}

// Observe implements Plugin.
func (CNPG) Observe(ctx context.Context, r client.Reader, svc *v1alpha1.SiteService, site string, now time.Time) observe.Report {
	role := svc.Spec.Plugin.CNPG.Role
	rep := observe.Report{Members: map[string]observe.Observation{}}
	declared := map[string]bool{}
	for _, m := range LocalMembers(svc, site) {
		o, decl := observeCluster(ctx, r, svc.Namespace, m, role, now)
		o.Site = site
		rep.Members[m.Name] = o
		if decl != "" {
			declared[decl] = true
		}
	}
	// Local clusters name the primary by its self name; map it to a member.
	var names []string
	for d := range declared {
		name := d
		for _, m := range svc.Spec.Members {
			if m.CNPG != nil && m.CNPG.Cluster == d {
				name = m.Name
			}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 1 {
		rep.Declared = names[0]
	}
	return rep
}

// cluster is the part of a CloudNativePG Cluster that the plugin reads.
type cluster struct {
	name, self, declared            string
	replica                         bool
	phase, currentPrimary, target   string
	demoting                        bool
	promotionToken, lastPromotionTk string
	// writable is the operator's last report that the current primary instance is out of recovery.
	writable bool
}

func readCluster(u *unstructured.Unstructured) cluster {
	c := cluster{name: u.GetName()}
	c.self, _, _ = unstructured.NestedString(u.Object, "spec", "replica", "self")
	if c.self == "" {
		c.self = c.name
	}
	_, hasReplica, _ := unstructured.NestedMap(u.Object, "spec", "replica")
	primary, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "primary")
	enabled, hasEnabled, _ := unstructured.NestedBool(u.Object, "spec", "replica", "enabled")
	switch {
	case !hasReplica:
		c.declared = c.self
	case hasEnabled:
		c.replica = enabled
		if !enabled {
			c.declared = c.self
		}
	default:
		c.declared = primary
		c.replica = primary != c.self
	}
	c.promotionToken, _, _ = unstructured.NestedString(u.Object, "spec", "replica", "promotionToken")
	c.phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	c.currentPrimary, _, _ = unstructured.NestedString(u.Object, "status", "currentPrimary")
	c.target, _, _ = unstructured.NestedString(u.Object, "status", "targetPrimary")
	c.demoting, _, _ = unstructured.NestedBool(u.Object, "status", "switchReplicaClusterStatus", "inProgress")
	c.lastPromotionTk, _, _ = unstructured.NestedString(u.Object, "status", "lastPromotionToken")
	if c.currentPrimary != "" {
		c.writable, _, _ = unstructured.NestedBool(u.Object, "status", "instancesReportedState", c.currentPrimary, "isPrimary")
	}
	return c
}

// transition returns why the cluster is moving, or "".
func (c cluster) transition() string {
	switch {
	case slices.Contains(transitioningPhases, c.phase):
		return "Phase: " + c.phase
	case c.currentPrimary != c.target:
		return "PrimaryChanging"
	case c.demoting:
		return "Demoting"
	case c.promotionToken != "" && c.promotionToken != c.lastPromotionTk:
		return "Promoting"
	case !c.replica && !c.writable:
		return "NotYetWritable"
	case c.replica && c.writable:
		return "NotYetReadOnly"
	}
	return ""
}

// observeCluster returns the member's observation and the cluster's declaration (a self name, or "").
func observeCluster(ctx context.Context, r client.Reader, namespace string, m v1alpha1.Member, role v1alpha1.CNPGRole, now time.Time) (observe.Observation, string) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgCluster)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: m.CNPG.Cluster}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return observe.Observation{Member: m.Name, Role: v1alpha1.RoleUnknown, State: v1alpha1.StateStable, Reason: "ClusterNotFound", ObservedAt: now}, ""
		}
		return observe.Unknown(m.Name, "", "ReadFailed: "+err.Error(), now), ""
	}
	c := readCluster(u)
	o := observe.Observation{Member: m.Name, Role: v1alpha1.RolePrimary, State: v1alpha1.StateStable, ObservedAt: now}
	if c.replica {
		o.Role = v1alpha1.RoleReplica
	}
	if t := c.transition(); t != "" {
		o.State = v1alpha1.StateTransitioning
		o.Reason = t
	}

	service := endpointService(c, role)
	if m.CNPG.Pooler != "" {
		ok, reason, err := poolerUsable(ctx, r, namespace, m.CNPG.Pooler, c.name, role)
		if err != nil {
			return observe.Unknown(m.Name, "", "ReadFailed: "+err.Error(), now), c.declared
		}
		if !ok {
			o.Reason = reason
			o.Endpoint = observe.Endpoint{Service: m.CNPG.Pooler, ServicePort: postgresPort}
			return o, c.declared
		}
		service = m.CNPG.Pooler
	}
	ep, err := ReadyEndpoint(ctx, r, namespace, service, postgresPort)
	o.Endpoint = ep
	switch {
	case errors.Is(err, ErrNoService):
		setReason(&o, "ServiceNotFound")
		return o, c.declared
	case err != nil:
		return observe.Unknown(m.Name, "", "ReadFailed: "+err.Error(), now), c.declared
	case len(ep.Addresses) == 0:
		setReason(&o, "NoReadyEndpoint")
		return o, c.declared
	}
	if o.State != v1alpha1.StateStable {
		return o, c.declared
	}
	switch role {
	case v1alpha1.CNPGRolePrimary:
		o.Eligible = o.Role == v1alpha1.RolePrimary
		o.Reason = "PrimaryCluster"
		if !o.Eligible {
			o.Reason = "ReplicaCluster"
		}
	default:
		o.Eligible = true
		o.Reason = "Ready"
	}
	return o, c.declared
}

func setReason(o *observe.Observation, reason string) {
	if o.Reason == "" {
		o.Reason = reason
	}
}

// endpointService returns the cluster's Service for the role.
func endpointService(c cluster, role v1alpha1.CNPGRole) string {
	switch role {
	case v1alpha1.CNPGRolePrimary:
		return c.name + "-rw"
	case v1alpha1.CNPGRoleReplica:
		if c.replica {
			return c.name + "-r"
		}
		return c.name + "-ro"
	}
	return c.name + "-r"
}

// poolerTypes is the Pooler type that matches each role.
var poolerTypes = map[v1alpha1.CNPGRole]string{
	v1alpha1.CNPGRolePrimary: "rw",
	v1alpha1.CNPGRoleReplica: "ro",
	v1alpha1.CNPGRoleAny:     "r",
}

// poolerUsable reports whether the Pooler belongs to the cluster, matches the role, and is not paused.
func poolerUsable(ctx context.Context, r client.Reader, namespace, name, clusterName string, role v1alpha1.CNPGRole) (bool, string, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgPooler)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "PoolerNotFound", nil
		}
		return false, "", err
	}
	owner, _, _ := unstructured.NestedString(u.Object, "spec", "cluster", "name")
	if owner != clusterName {
		return false, "PoolerOfAnotherCluster", nil
	}
	typ, _, _ := unstructured.NestedString(u.Object, "spec", "type")
	if typ == "" {
		typ = "rw"
	}
	if typ != poolerTypes[role] {
		return false, "PoolerTypeMismatch", nil
	}
	if paused, _, _ := unstructured.NestedBool(u.Object, "spec", "pgbouncer", "paused"); paused {
		return false, "PoolerPaused", nil
	}
	return true, "", nil
}
