package xds

import (
	"encoding/json"
	"slices"
	"testing"

	bootstrap "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// staticConfig renders a snapshot as a static Envoy bootstrap: EDS clusters become STATIC with their assignment inlined.
func staticConfig(snap *cache.Snapshot) ([]byte, error) {
	b := &bootstrap.Bootstrap{
		Node:            &core.Node{Id: "strait-gateway-0", Cluster: NodeCluster},
		Admin:           &bootstrap.Admin{Address: socketAddress("127.0.0.1", AdminPort)},
		StaticResources: &bootstrap.Bootstrap_StaticResources{},
	}
	ls := snap.GetResources(resource.ListenerType)
	for _, name := range sortedKeys(ls) {
		b.StaticResources.Listeners = append(b.StaticResources.Listeners, ls[name].(*listener.Listener))
	}
	cs := snap.GetResources(resource.ClusterType)
	clas := snap.GetResources(resource.EndpointType)
	for _, name := range sortedKeys(cs) {
		c := proto.Clone(cs[name]).(*cluster.Cluster)
		if c.GetType() == cluster.Cluster_EDS {
			c.ClusterDiscoveryType = &cluster.Cluster_Type{Type: cluster.Cluster_STATIC}
			c.EdsClusterConfig = nil
			c.LoadAssignment = clas[name].(*endpoint.ClusterLoadAssignment)
		}
		b.StaticResources.Clusters = append(b.StaticResources.Clusters, c)
	}
	return protojson.MarshalOptions{Multiline: true}.Marshal(b)
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestStaticConfig(t *testing.T) {
	out, err := staticConfig(build(t, collect(t)))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		StaticResources struct {
			Listeners []json.RawMessage `json:"listeners"`
			Clusters  []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"clusters"`
		} `json:"staticResources"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.StaticResources.Listeners) != 6 || len(v.StaticResources.Clusters) != 6 {
		t.Fatalf("%d listeners, %d clusters", len(v.StaticResources.Listeners), len(v.StaticResources.Clusters))
	}
	for _, c := range v.StaticResources.Clusters {
		if c.Type == "EDS" {
			t.Errorf("%s is still EDS", c.Name)
		}
	}
}
