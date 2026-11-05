package gateway

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Probes serves /healthz, 200 unless alive returns an error, and /readyz, 200 while Envoy's admin /ready answers 200.
// A nil alive always passes.
func Probes(envoyAdmin string, c *http.Client, alive func(time.Time) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if alive != nil {
			if err := alive(time.Now()); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := EnvoyReady(r.Context(), c, envoyAdmin); err != nil {
			http.Error(w, "envoy is not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// EnvoyReady returns nil while Envoy's admin /ready answers 200 within a second.
func EnvoyReady(ctx context.Context, c *http.Client, envoyAdmin string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := get(ctx, c, envoyAdmin+"/ready", 1<<10)
	return err
}
