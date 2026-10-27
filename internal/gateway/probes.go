package gateway

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Probes serves /healthz, 200 while the process runs, and /readyz, 200 while Envoy's admin /ready answers 200.
func Probes(envoyAdmin string, c *http.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if _, err := get(ctx, c, envoyAdmin+"/ready", 1<<10); err != nil {
			http.Error(w, "envoy is not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
