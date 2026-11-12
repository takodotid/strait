package main

import (
	"log/slog"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/takodotid/strait/internal/gateway"
)

// The agent's requests to the API server end within gateway.APITimeout, 5s. Its cache's config keeps no timeout,
// which would end its watches (RFC0005, requirement 49; issue #61).
func TestAPIConfig(t *testing.T) {
	cfg := &rest.Config{Host: "https://192.0.2.1:6443"}
	api := apiConfig(cfg)
	if api.Timeout != gateway.APITimeout || gateway.APITimeout != 5*time.Second {
		t.Errorf("timeout = %s, want %s", api.Timeout, 5*time.Second)
	}
	if cfg.Timeout != 0 {
		t.Errorf("the cache's config has the timeout %s, want none", cfg.Timeout)
	}
}

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

func TestLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"error": slog.LevelError, "info": slog.LevelInfo, "debug": slog.LevelDebug} {
		if got, err := logLevel(in); err != nil || got != want {
			t.Errorf("logLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := logLevel("verbose"); err == nil {
		t.Error("logLevel(verbose) has no error")
	}
}
