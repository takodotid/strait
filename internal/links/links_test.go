package links

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	straitv1 "github.com/takodotid/strait/api/v1"
)

func link(name, peer string, cost int32) straitv1.SiteLink {
	return straitv1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: straitv1.SiteLinkSpec{Peer: peer, Cost: cost}}
}

func TestByPeer(t *testing.T) {
	got := ByPeer([]straitv1.SiteLink{link("wan", "b", 100), link("lan-2", "b", 10), link("lan-1", "b", 10), link("x", "c", 5)})
	var names []string
	for _, l := range got["b"] {
		names = append(names, l.Name)
	}
	if want := []string{"lan-1", "lan-2", "wan"}; len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("order = %v, want %v", names, want)
	}
	if len(got["c"]) != 1 {
		t.Fatalf("peer c has %d links", len(got["c"]))
	}
}

func TestInterface(t *testing.T) {
	n := Interface("a-to-b-wan")
	if len(n) != 10 || n[:2] != "st" || n != Interface("a-to-b-wan") || n == Interface("a-to-b-lan") {
		t.Fatalf("Interface = %q", n)
	}
}
