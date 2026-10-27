package exchange

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestHandler(t *testing.T) {
	h := NewHandler(func() Document { return exampleLocal().Document("a", at) })
	tests := []struct {
		method, target string
		code           int
	}{
		{http.MethodGet, Path, http.StatusOK},
		{http.MethodGet, Path + "?watch=true", http.StatusOK},
		{http.MethodGet, HealthPath, http.StatusOK},
		{http.MethodHead, Path, http.StatusMethodNotAllowed},
		{http.MethodPost, Path, http.StatusMethodNotAllowed},
		{http.MethodPut, Path, http.StatusMethodNotAllowed},
		{http.MethodPatch, Path, http.StatusMethodNotAllowed},
		{http.MethodDelete, Path, http.StatusMethodNotAllowed},
		{http.MethodOptions, Path, http.StatusMethodNotAllowed},
		{http.MethodPost, HealthPath, http.StatusMethodNotAllowed},
		{http.MethodHead, HealthPath, http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodGet, Path + "/", http.StatusNotFound},
		{http.MethodGet, "/v1alpha1/", http.StatusNotFound},
		{http.MethodGet, "/v1alpha1//observations", http.StatusNotFound},
		{http.MethodGet, "/x/../healthz", http.StatusNotFound},
		{http.MethodPost, "/other", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.target, nil))
			if w.Code != tt.code {
				t.Fatalf("want %d, have %d", tt.code, w.Code)
			}
			if tt.code == http.StatusMethodNotAllowed && w.Header().Get("Allow") != http.MethodGet {
				t.Errorf("Allow header: %q", w.Header().Get("Allow"))
			}
		})
	}
}

func TestHandlerBodies(t *testing.T) {
	srv := httptest.NewServer(NewHandler(func() Document { return exampleLocal().Document("a", at) }))
	defer srv.Close()

	resp, err := http.Get(srv.URL + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("healthz body: %q", body)
	}

	resp, err = http.Get(srv.URL + Path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: %q", ct)
	}
	var have, want map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&have); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rfcExample), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("document\nwant %v\nhave %v", want, have)
	}
}
