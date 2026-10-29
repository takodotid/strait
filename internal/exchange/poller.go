package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

// idle is the wait between two rounds while the poll interval is not set.
const idle = time.Second

// PeerState is the outcome of a peer's polls.
type PeerState struct {
	// LastSuccess is when the last good document arrived, from time.Now. Zero before the first.
	LastSuccess time.Time
	// LastError is the error of the last failed poll.
	LastError string
	// LastErrorTime is when the last poll failed.
	LastErrorTime time.Time
	// Mismatch is true while the last answer came from another site. The peer is unreachable until a good document arrives.
	Mismatch bool
}

// Fresh reports whether the last good document is at most timeout old at now.
// now must come from time.Now, so that the age is measured on the monotonic clock.
func (s PeerState) Fresh(now time.Time, timeout time.Duration) bool {
	return !s.LastSuccess.IsZero() && !s.Mismatch && now.Sub(s.LastSuccess) <= timeout
}

type peer struct {
	state    PeerState
	document Document
	// content is the document without generatedAt, to detect a change.
	content []byte
	// fresh is the freshness last reported to Changed.
	fresh bool
}

// Poller polls every peer's document. Start runs it.
type Poller struct {
	// Peers returns the names of the peers to poll. Read every round.
	Peers func() []string
	// URL returns a peer's base URL. Path is appended to it.
	URL func(peer string) (string, error)
	// Exchange returns the poll interval and the peer timeout. Read every round.
	Exchange func() straitv1.ExchangeSpec
	// Changed is called when a peer's document changes, apart from generatedAt, or its freshness flips.
	// It runs on the poller's goroutine and must not block.
	Changed func(peer string)
	// Client sends the polls. When nil, a client with no proxy, no keep-alive and no redirect is used.
	Client *http.Client

	mu    sync.Mutex
	peers map[string]*peer
}

// Start polls every peer until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) error {
	if p.Peers == nil || p.URL == nil || p.Exchange == nil {
		return errors.New("exchange: Peers, URL and Exchange must be set")
	}
	if p.Client == nil {
		p.Client = newClient()
	}
	for {
		start := time.Now()
		ex := p.Exchange()
		wait := ex.PollInterval.Duration
		if wait > 0 {
			p.round(ctx, wait, ex.PeerTimeout.Duration)
		} else {
			wait = idle
		}
		timer := time.NewTimer(time.Until(start.Add(wait)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// newClient returns a client that ignores proxy settings, opens a connection per poll, so that each poll follows the
// gateway's current link choice, and never follows a redirect.
func newClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// round polls every peer once, each poll timing out within interval.
func (p *Poller) round(ctx context.Context, interval, timeout time.Duration) {
	names := p.Peers()
	listed := map[string]bool{}
	for _, n := range names {
		listed[n] = true
	}
	p.mu.Lock()
	if p.peers == nil {
		p.peers = map[string]*peer{}
	}
	var changed []string
	for n, s := range p.peers {
		if !listed[n] {
			if s.fresh {
				changed = append(changed, n)
			}
			delete(p.peers, n)
		}
	}
	for n := range listed {
		if p.peers[n] == nil {
			p.peers[n] = &peer{}
		}
	}
	changed = append(changed, p.flips(time.Now(), timeout)...)
	p.mu.Unlock()
	p.notify(changed)

	results := make(chan result, len(listed))
	var wg sync.WaitGroup
	for n := range listed {
		wg.Go(func() {
			doc, content, err := p.fetch(ctx, n, interval)
			results <- result{peer: n, document: doc, content: content, err: err, at: time.Now()}
		})
	}
	wg.Wait()
	close(results)

	changed = nil
	p.mu.Lock()
	for r := range results {
		s := p.peers[r.peer]
		if r.err != nil {
			if ctx.Err() != nil {
				continue
			}
			s.state.LastError = r.err.Error()
			s.state.LastErrorTime = r.at
			if errors.Is(r.err, errSiteMismatch) {
				s.state.Mismatch = true
			}
			continue
		}
		if !bytes.Equal(s.content, r.content) {
			changed = append(changed, r.peer)
		}
		s.document = r.document
		s.content = r.content
		s.state.LastSuccess = r.at
		s.state.Mismatch = false
	}
	changed = append(changed, p.flips(time.Now(), timeout)...)
	p.mu.Unlock()
	p.notify(changed)
}

type result struct {
	peer     string
	document Document
	content  []byte
	err      error
	at       time.Time
}

// flips returns the peers whose freshness changed since it was last reported, and records it. p.mu must be held.
func (p *Poller) flips(now time.Time, timeout time.Duration) []string {
	var out []string
	for n, s := range p.peers {
		fresh := s.state.Fresh(now, timeout)
		if fresh != s.fresh {
			s.fresh = fresh
			out = append(out, n)
		}
	}
	return out
}

func (p *Poller) notify(peers []string) {
	if p.Changed == nil {
		return
	}
	seen := map[string]bool{}
	for _, n := range peers {
		if !seen[n] {
			seen[n] = true
			p.Changed(n)
		}
	}
}

// fetch polls one peer's document.
func (p *Poller) fetch(ctx context.Context, peer string, interval time.Duration) (Document, []byte, error) {
	base, err := p.URL(peer)
	if err != nil {
		return Document{}, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, interval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+Path, nil)
	if err != nil {
		return Document{}, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return Document{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Document{}, nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxDocumentSize {
		return Document{}, nil, fmt.Errorf("document larger than %d bytes", MaxDocumentSize)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocumentSize+1))
	if err != nil {
		return Document{}, nil, err
	}
	if len(body) > MaxDocumentSize {
		return Document{}, nil, fmt.Errorf("document larger than %d bytes", MaxDocumentSize)
	}
	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return Document{}, nil, fmt.Errorf("decode document: %w", err)
	}
	if err := doc.validate(peer); err != nil {
		return Document{}, nil, err
	}
	stripped := doc
	stripped.GeneratedAt = time.Time{}
	content, err := json.Marshal(stripped)
	if err != nil {
		return Document{}, nil, err
	}
	return doc, content, nil
}

// Fresh reports whether a peer's last good document is at most timeout old at now. now must come from time.Now.
func (p *Poller) Fresh(peer string, now time.Time, timeout time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.peers[peer]
	return ok && s.state.Fresh(now, timeout)
}

// Snapshot returns a peer's last good document, which may be stale, and its poll state.
// The document is shared and must not be modified.
func (p *Poller) Snapshot(peer string) (Document, PeerState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.peers[peer]
	if !ok {
		return Document{}, PeerState{}
	}
	return s.document, s.state
}

// View returns what every fresh peer reports about one SiteService, by peer name. now must come from time.Now.
// A peer's report holds only the observations of members that the spec places at that peer.
// A stale peer is left out, so its members count as Unknown and its declaration as unknown.
func (p *Poller) View(namespace, name string, members []straitv1.Member, now time.Time, timeout time.Duration) map[string]observe.Report {
	site := map[string]string{}
	for _, m := range members {
		site[m.Name] = m.Site
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]observe.Report{}
	for n, s := range p.peers {
		if !s.state.Fresh(now, timeout) {
			continue
		}
		r := observe.Report{Members: map[string]observe.Observation{}}
		if svc, ok := s.document.Service(namespace, name); ok {
			r.Declared = svc.DeclaredPrimary
			for _, m := range svc.Members {
				if site[m.Name] == n {
					r.Members[m.Name] = m.Observation(n)
				}
			}
		}
		out[n] = r
	}
	return out
}
