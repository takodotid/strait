package main

import "testing"

// The probe server binds to the address that --probe-ip gives, the pod's own in the chart, so that a peer cannot reach
// it through a WireGuard interface (RFC0007, requirement 12).
func TestProbeListenAddr(t *testing.T) {
	for _, tc := range []struct {
		addr, ip, want string
		err            bool
	}{
		{":8090", "", ":8090", false},
		{":8090", "10.0.0.5", "10.0.0.5:8090", false},
		{"0.0.0.0:8090", "10.0.0.5", "10.0.0.5:8090", false},
		{":8090", "fd00::5", "[fd00::5]:8090", false},
		{":8090", "gateway-0", "", true},
		{"8090", "10.0.0.5", "", true},
	} {
		got, err := probeListenAddr(tc.addr, tc.ip)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("probeListenAddr(%q, %q) = %q, %v; want %q, error %v", tc.addr, tc.ip, got, err, tc.want, tc.err)
		}
	}
}
