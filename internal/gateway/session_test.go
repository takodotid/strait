package gateway

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/takodotid/strait/internal/links"
)

// counters is one read of a peer's counters, at an offset from the start of a test.
type counters struct {
	at     time.Duration
	rx, tx int64
}

// run feeds reads of one interface's peer to check, and drops the session when check says so, as the agent does.
// It returns the offsets of the drops and of the answers after a drop.
func run(w Interface, reads []counters) (drops, answers []time.Duration) {
	s := sessions{}
	t0 := time.Unix(1_000_000, 0)
	for _, r := range reads {
		now := t0.Add(r.at)
		dev := Device{Name: w.Name, Up: true, Peers: []wgtypes.Peer{{PublicKey: w.Peer, ReceiveBytes: r.rx, TransmitBytes: r.tx}}}
		drop, answered := s.check([]Interface{w}, []Device{dev}, now)
		for range answered {
			answers = append(answers, r.at)
		}
		for _, st := range drop {
			drops = append(drops, r.at)
			s.dropped(st.W.Name, now)
		}
	}
	return drops, answers
}

func dialing(t *testing.T) Interface {
	return Interface{Name: "st1", Link: "to-b", Peer: newKey(t).PublicKey(), Endpoint: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 51820}, PeerIP: netip.MustParsePrefix("100.64.0.2/32")}
}

// silentPeer is a peer that answers until 1s, then sends nothing, while this side sends at every read. Counters start
// again at zero after each drop.
func silentPeer(drops []time.Duration) []counters {
	var out []counters
	rx, tx := int64(1000), int64(1000)
	for at := time.Duration(0); at <= 9*time.Second; at += SessionInterval {
		if slices.Contains(drops, at-SessionInterval) {
			rx, tx = 0, 0
		}
		tx += 100
		if at <= time.Second {
			rx += 100
		}
		out = append(out, counters{at, rx, tx})
	}
	return out
}

// RFC0005 requirement 37: a dialing link whose peer stays silent while this side sends is dropped after StaleSession,
// then again every StaleRetry, and not before.
func TestStaleSessionDroppedAfter2sThenEverySecond(t *testing.T) {
	w := dialing(t)
	// The drops change the counters, so the reads follow the drops that the rule is expected to make.
	want := []time.Duration{3 * time.Second, 4 * time.Second, 5 * time.Second, 6 * time.Second, 7 * time.Second, 8 * time.Second, 9 * time.Second}
	drops, answers := run(w, silentPeer(want))
	if !slices.Equal(drops, want) {
		t.Fatalf("drops at %v, want %v", drops, want)
	}
	if len(answers) != 0 {
		t.Fatalf("answers at %v from a silent peer", answers)
	}
}

// RFC0005 requirement 37: a packet from the peer ends the stall.
func TestStaleSessionEndsWhenThePeerAnswers(t *testing.T) {
	w := dialing(t)
	reads := []counters{
		{0, 1000, 1000},
		{time.Second, 1000, 1100},
		{2 * time.Second, 1000, 1200}, // silent for 2s while sending: drop, and the counters start again
		{2250 * time.Millisecond, 0, 148},
		{2750 * time.Millisecond, 0, 148},
		{3 * time.Second, 0, 148}, // 1s after the drop: drop again
		{3250 * time.Millisecond, 0, 148},
		{3500 * time.Millisecond, 92, 148}, // the handshake's answer
		{4500 * time.Millisecond, 92, 400}, // 1s after the answer, sending: no drop
		{5500 * time.Millisecond, 92, 500}, // 2s after the answer: a new stall
		{6 * time.Second, 0, 148},
		{6500 * time.Millisecond, 300, 600}, // answers
		{8 * time.Second, 300, 600},         // nothing sent: no drop
		{20 * time.Second, 300, 600},
	}
	drops, answers := run(w, reads)
	if want := []time.Duration{2 * time.Second, 3 * time.Second, 5500 * time.Millisecond}; !slices.Equal(drops, want) {
		t.Errorf("drops at %v, want %v", drops, want)
	}
	if want := []time.Duration{3500 * time.Millisecond, 6500 * time.Millisecond}; !slices.Equal(answers, want) {
		t.Errorf("answers at %v, want %v", answers, want)
	}
}

// RFC0005 requirement 37: a link that waits for its peer to dial, a live link and an idle link are never dropped.
func TestStaleSessionNeverDropped(t *testing.T) {
	waiting := dialing(t)
	waiting.Endpoint = nil
	if drops, _ := run(waiting, silentPeer(nil)); len(drops) != 0 {
		t.Errorf("waiting link: drops at %v", drops)
	}

	var live, idle []counters
	rx := int64(0)
	for at := time.Duration(0); at <= 30*time.Second; at += SessionInterval {
		if at%time.Second == 0 {
			rx += 60
		}
		live = append(live, counters{at, rx, int64(at / SessionInterval)})
		idle = append(idle, counters{at, 500, 500})
	}
	if drops, _ := run(dialing(t), live); len(drops) != 0 {
		t.Errorf("live link: drops at %v", drops)
	}
	if drops, _ := run(dialing(t), idle); len(drops) != 0 {
		t.Errorf("idle link: drops at %v", drops)
	}
}

// RFC0005 requirement 37: the agent drops the session of a silent dialing link in one change that keeps the peer's
// endpoint, allowed IP and keepalive, then sends a packet to the peer's tunnel address. It leaves a link that waits for
// its peer as it is.
func TestRenewStaleReplacesThePeer(t *testing.T) {
	key := newKey(t)
	toB := wgLink("to-b", "b", 100, 51820, "192.0.2.10:51820", "100.64.0.1/30", "100.64.0.2")
	toC := wgLink("to-c", "c", 100, 51821, "", "100.64.1.1/30", "100.64.1.2")
	pb, pc := peer("b", newKey(t).PublicKey().String()), peer("c", newKey(t).PublicKey().String())
	a, k, _ := newAgent(t, testSite(), keySecret(key.String()), &toB, &toC, &pb, &pc)
	var poked []netip.Addr
	a.Poke = func(p netip.Addr) error { poked = append(poked, p); return nil }
	ctx := context.Background()
	a.syncWireGuard(ctx)
	ifB, ifC := links.Interface("to-b"), links.Interface("to-c")
	k.takeCalls()

	set := func(rx, tx int64) {
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, name := range []string{ifB, ifC} {
			p := &k.devs[name].Peers[0]
			p.ReceiveBytes, p.TransmitBytes = rx, tx
			if name == ifC {
				p.Endpoint = &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 40000}
			}
		}
	}
	t0 := time.Now()
	s := sessions{}
	step := func(at time.Duration) { a.renewStale(ctx, s, func() time.Time { return t0.Add(at) }) }
	set(1000, 1000)
	step(0)
	set(1000, 2000)
	step(time.Second)
	if calls := k.takeCalls(); len(calls) != 0 {
		t.Fatalf("calls before 2s of silence: %v", calls)
	}
	step(2 * time.Second)
	calls := k.takeCalls()
	want := "configure " + ifB + " replace-peers add-peer allowed-ips endpoint=192.0.2.10:51820 keepalive=25s"
	if !slices.Equal(calls, []string{want}) {
		t.Fatalf("calls after 2s of silence: %v, want %q", calls, want)
	}
	if want := []netip.Addr{netip.MustParseAddr("100.64.0.2")}; !slices.Equal(poked, want) {
		t.Errorf("packets sent to %v, want %v", poked, want)
	}
	devs, _ := k.Devices()
	for _, d := range devs {
		p := d.Peers[0]
		switch d.Name {
		case ifB:
			if p.Endpoint.String() != "192.0.2.10:51820" || len(p.AllowedIPs) != 1 || p.AllowedIPs[0].String() != "100.64.0.2/32" ||
				p.PersistentKeepaliveInterval != 25*time.Second || p.ReceiveBytes != 0 || p.TransmitBytes != 0 {
				t.Errorf("to-b's peer after the drop: %+v", p)
			}
		case ifC:
			if p.Endpoint.String() != "198.51.100.7:40000" || p.ReceiveBytes != 1000 {
				t.Errorf("to-c's peer changed: %+v", p)
			}
		}
	}
	// A sync after the drop changes nothing.
	a.syncWireGuard(ctx)
	if calls := k.takeCalls(); len(calls) != 0 {
		t.Fatalf("sync after the drop: %v", calls)
	}
}
