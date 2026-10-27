package exchange

import (
	"encoding/json"
	"net/http"
)

type handler struct {
	document func() Document
}

// NewHandler serves GET Path with the document that document returns, and GET HealthPath.
// It answers 405 to any other method on those paths, and 404 to any other path.
func NewHandler(document func() Document) http.Handler {
	return handler{document: document}
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Path && r.URL.Path != HealthPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == HealthPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
		return
	}
	body, err := json.Marshal(h.document())
	if err != nil {
		http.Error(w, "cannot encode the document", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
