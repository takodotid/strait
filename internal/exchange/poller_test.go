package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

// site is a peer's served document. Its handler checks the path the poller asks for.
type site struct {
	t    *testing.T
	name string
	mu   sync.Mutex
	// serve writes the answer. When nil, the site serves its document.
	serve func(w http.ResponseWriter, r *http.Request)
	doc   Document
}

func newSite(t *testing.T, name string, doc Document) (*site, *httptest.Server) {
	s := &site{t: t, name: name, doc: doc}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/peers/"+name+Path || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		serve, doc := s.serve, s.doc
		s.mu.Unlock()
		if serve != nil {
			serve(w, r)
			return
		}
		doc.GeneratedAt = time.Now().UTC()
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *site) set(doc Document, serve func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc
	s.serve = serve
}

func siteDocument(name string, members ...observe.Observation) Document {
	l := &Local{}
	l.SetService("app", "pg", "pg-a", members)
	l.SetExport("app", "pg-"+name, 30901, true)
	return l.Document(name, at)
}

// recorder records Changed calls.
type recorder struct {
	mu    sync.Mutex
	calls []string
	ch    chan string
}

func newRecorder() *recorder { return &recorder{ch: make(chan string, 100)} }

func (r *recorder) changed(peer string) {
	r.mu.Lock()
	r.calls = append(r.calls, peer)
	r.mu.Unlock()
	r.ch <- peer
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	for len(r.ch) > 0 {
		<-r.ch
	}
	return out
}

func exchangeSpec(interval, timeout time.Duration) func() v1alpha1.ExchangeSpec {
	return func() v1alpha1.ExchangeSpec {
		return v1alpha1.ExchangeSpec{PollInterval: metav1.Duration{Duration: interval}, PeerTimeout: metav1.Duration{Duration: timeout}}
	}
}

// newPoller polls the given servers by peer name.
func newPoller(servers map[string]*httptest.Server, rec *recorder) *Poller {
	p := &Poller{
		Peers: func() []string {
			var out []string
			for n := range servers {
				out = append(out, n)
			}
			return out
		},
		URL: func(peer string) (string, error) {
			return servers[peer].URL + "/peers/" + peer + "/", nil
		},
		Exchange: exchangeSpec(time.Second, 3*time.Second),
		Client:   newClient(),
	}
	if rec != nil {
		p.Changed = rec.changed
	}
	return p
}

func TestPollGoodDocument(t *testing.T) {
	doc := siteDocument("b", obs("pg-b", "b"))
	_, srv := newSite(t, "b", doc)
	p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
	before := time.Now()
	p.round(context.Background(), time.Second, 3*time.Second)

	got, state := p.Snapshot("b")
	if state.LastError != "" {
		t.Fatalf("error: %s", state.LastError)
	}
	if state.LastSuccess.Before(before) {
		t.Errorf("LastSuccess %v before the poll", state.LastSuccess)
	}
	got.GeneratedAt = doc.GeneratedAt
	if !equalJSON(t, got, doc) {
		t.Errorf("document\nwant %+v\nhave %+v", doc, got)
	}
	if !p.Fresh("b", time.Now(), 3*time.Second) {
		t.Errorf("peer not fresh after a good poll")
	}
	if p.Fresh("c", time.Now(), 3*time.Second) {
		t.Errorf("unknown peer is fresh")
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ja, jb)
}

func TestPollSiteMismatch(t *testing.T) {
	s, srv := newSite(t, "b", siteDocument("b", obs("pg-b", "b")))
	p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
	p.round(context.Background(), time.Second, time.Minute)
	_, good := p.Snapshot("b")
	if !p.Fresh("b", time.Now(), time.Minute) {
		t.Fatalf("peer not fresh after a good poll: %s", good.LastError)
	}

	s.set(siteDocument("c", obs("pg-c", "c")), nil)
	p.round(context.Background(), time.Second, time.Minute)
	doc, state := p.Snapshot("b")
	if !strings.Contains(state.LastError, "site mismatch") {
		t.Errorf("want a site mismatch, have %q", state.LastError)
	}
	if p.Fresh("b", time.Now(), time.Minute) {
		t.Errorf("peer still fresh after a site mismatch")
	}
	if !state.LastSuccess.Equal(good.LastSuccess) || doc.Site != "b" {
		t.Errorf("site mismatch replaced the last good poll")
	}
	if len(p.View("app", "pg", []v1alpha1.Member{{Name: "pg-b", Site: "b"}}, time.Now(), time.Minute)) != 0 {
		t.Errorf("view holds a peer that answered for another site")
	}

	s.set(siteDocument("b", obs("pg-b", "b")), nil)
	p.round(context.Background(), time.Second, time.Minute)
	if !p.Fresh("b", time.Now(), time.Minute) {
		t.Errorf("peer not fresh after a good poll following a mismatch")
	}
}

func TestPollRejects(t *testing.T) {
	valid, err := json.Marshal(siteDocument("b", obs("pg-b", "b")))
	if err != nil {
		t.Fatal(err)
	}
	padded := append(bytes.Repeat([]byte(" "), MaxDocumentSize+1-len(valid)), valid...)
	if err := json.Unmarshal(padded, &Document{}); err != nil {
		t.Fatalf("padded document does not parse: %v", err)
	}
	fits := append(bytes.Repeat([]byte(" "), MaxDocumentSize-len(valid)), valid...)
	wrong := func(change func(*Document)) []byte {
		d := siteDocument("b", obs("pg-b", "b"))
		change(&d)
		out, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tests := []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
		want  string
	}{
		{"oversize, chunked", func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			_, _ = w.Write(padded)
		}, "larger than"},
		{"oversize, with length", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(padded)))
			_, _ = w.Write(padded)
		}, "larger than"},
		{"apiVersion", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(wrong(func(d *Document) { d.APIVersion = "strait.tako.id/v1" }))
		}, "unexpected document"},
		{"kind", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(wrong(func(d *Document) { d.Kind = "Status" }))
		}, "unexpected document"},
		{"status", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}, "unexpected status 503"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, "unexpected status 302"},
		{"not JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		}, "decode document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site, srv := newSite(t, "b", Document{})
			site.set(Document{}, tt.serve)
			p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
			p.round(context.Background(), 5*time.Second, time.Minute)
			_, state := p.Snapshot("b")
			if !strings.Contains(state.LastError, tt.want) {
				t.Errorf("want error %q, have %q", tt.want, state.LastError)
			}
			if !state.LastSuccess.IsZero() || p.Fresh("b", time.Now(), time.Minute) {
				t.Errorf("a rejected document counts as a success")
			}
		})
	}

	t.Run("fits", func(t *testing.T) {
		site, srv := newSite(t, "b", Document{})
		site.set(Document{}, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(fits) })
		p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
		p.round(context.Background(), 5*time.Second, time.Minute)
		if _, state := p.Snapshot("b"); state.LastError != "" {
			t.Errorf("a document of exactly %d bytes is rejected: %s", MaxDocumentSize, state.LastError)
		}
	})
}

func TestFailureKeepsFresh(t *testing.T) {
	s, srv := newSite(t, "b", siteDocument("b", obs("pg-b", "b")))
	p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
	p.round(context.Background(), time.Second, time.Minute)
	s.set(Document{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	p.round(context.Background(), time.Second, time.Minute)
	if _, state := p.Snapshot("b"); state.LastError == "" || !p.Fresh("b", time.Now(), time.Minute) {
		t.Errorf("a failed poll made the peer stale before its timeout: %+v", state)
	}
}

func TestPollTimeout(t *testing.T) {
	site, srv := newSite(t, "b", Document{})
	site.set(Document{}, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	p := newPoller(map[string]*httptest.Server{"b": srv}, nil)
	start := time.Now()
	p.round(context.Background(), 100*time.Millisecond, time.Minute)
	if d := time.Since(start); d > time.Second {
		t.Errorf("round took %s with a 100ms interval", d)
	}
	_, state := p.Snapshot("b")
	if !strings.Contains(state.LastError, "deadline exceeded") {
		t.Errorf("want a timeout, have %q", state.LastError)
	}
	if state.LastErrorTime.IsZero() {
		t.Errorf("LastErrorTime not set")
	}
}

func TestPollURLError(t *testing.T) {
	errNoGateway := errors.New("no active gateway")
	p := &Poller{Peers: func() []string { return []string{"b"} }}
	p.URL = func(string) (string, error) { return "", errNoGateway }
	p.round(context.Background(), time.Second, time.Minute)
	if _, state := p.Snapshot("b"); state.LastError != errNoGateway.Error() {
		t.Errorf("want %q, have %q", errNoGateway, state.LastError)
	}
}

func TestChanged(t *testing.T) {
	rec := newRecorder()
	s, srv := newSite(t, "b", siteDocument("b", obs("pg-b", "b")))
	p := newPoller(map[string]*httptest.Server{"b": srv}, rec)
	round := func() { p.round(context.Background(), time.Second, time.Minute) }

	round()
	if got := rec.take(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("first document: want one call, have %v", got)
	}
	// The site serves a new generatedAt on every poll.
	round()
	if got := rec.take(); len(got) != 0 {
		t.Errorf("same content: want no call, have %v", got)
	}
	o := obs("pg-b", "b")
	o.ObservedAt = at.Add(time.Second)
	s.set(siteDocument("b", o), nil)
	round()
	if got := rec.take(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("observedAt changed: want one call, have %v", got)
	}
	o.State = v1alpha1.StateTransitioning
	s.set(siteDocument("b", o), nil)
	round()
	if got := rec.take(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("state changed: want one call, have %v", got)
	}
	s.set(siteDocument("c", o), nil)
	round()
	if got := rec.take(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("site mismatch: want one call, have %v", got)
	}
}

func TestFreshnessFlips(t *testing.T) {
	rec := newRecorder()
	s, srv := newSite(t, "b", siteDocument("b", obs("pg-b", "b")))
	p := newPoller(map[string]*httptest.Server{"b": srv}, rec)
	p.Exchange = exchangeSpec(20*time.Millisecond, 60*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start: %v", err)
		}
	}()

	wait := func(want bool) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-rec.ch:
				if p.Fresh("b", time.Now(), 60*time.Millisecond) == want {
					return
				}
			case <-deadline:
				t.Fatalf("peer did not become fresh=%v", want)
			}
		}
	}
	wait(true)

	s.set(Document{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	wait(false)

	s.set(siteDocument("b", obs("pg-b", "b")), nil)
	wait(true)
}

func TestPeerRemoved(t *testing.T) {
	rec := newRecorder()
	_, srv := newSite(t, "b", siteDocument("b", obs("pg-b", "b")))
	p := newPoller(map[string]*httptest.Server{"b": srv}, rec)
	p.round(context.Background(), time.Second, time.Minute)
	rec.take()

	p.Peers = func() []string { return nil }
	p.round(context.Background(), time.Second, time.Minute)
	if got := rec.take(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("removed fresh peer: want one call, have %v", got)
	}
	if doc, state := p.Snapshot("b"); doc.Site != "" || !state.LastSuccess.IsZero() {
		t.Errorf("removed peer still has state")
	}
	if p.Fresh("b", time.Now(), time.Minute) {
		t.Errorf("removed peer is fresh")
	}
}

func TestView(t *testing.T) {
	members := []v1alpha1.Member{{Name: "pg-a", Site: "a"}, {Name: "pg-b", Site: "b"}, {Name: "pg-c", Site: "c"}}
	_, b := newSite(t, "b", siteDocument("b", obs("pg-b", "b"), obs("pg-a", "b"), obs("pg-x", "b")))
	_, c := newSite(t, "c", siteDocument("c", obs("pg-c", "c")))
	l := &Local{}
	l.SetService("app", "other", "", []observe.Observation{obs("other-d", "d")})
	_, d := newSite(t, "d", l.Document("d", at))
	p := newPoller(map[string]*httptest.Server{"b": b, "c": c, "d": d}, nil)
	p.round(context.Background(), time.Second, time.Minute)

	// c is stale: its last success is older than the timeout.
	p.mu.Lock()
	p.peers["c"].state.LastSuccess = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()

	view := p.View("app", "pg", members, time.Now(), time.Minute)
	if _, ok := view["c"]; ok {
		t.Errorf("stale peer in the view")
	}
	rb, ok := view["b"]
	if !ok {
		t.Fatalf("fresh peer missing from the view: %v", view)
	}
	if rb.Declared != "pg-a" {
		t.Errorf("declared: %q", rb.Declared)
	}
	if len(rb.Members) != 1 {
		t.Errorf("want only pg-b, have %v", rb.Members)
	}
	if o := rb.Members["pg-b"]; o.Site != "b" || o.State != v1alpha1.StateStable || !o.Eligible {
		t.Errorf("pg-b: %+v", o)
	}
	rd, ok := view["d"]
	if !ok || rd.Declared != "" || len(rd.Members) != 0 {
		t.Errorf("fresh peer without the service: %+v, %v", rd, ok)
	}
}
