package gateway

import (
	"net"
	"net/netip"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// StaleSession is how long a dialing link may send while its peer sends nothing before the agent drops the link's
// WireGuard session. The peer's health checks reach every live link at least once a second.
//
// After a handover at the peer's site, the peer's new gateway holds no session, and when its link waits for this side to
// dial, it cannot start one. WireGuard itself starts a new handshake only after 15s of sending with no answer.
const StaleSession = 2 * time.Second

// StaleRetry is how often the agent drops the session again while the peer stays silent. A handshake sent before the
// peer's site points its WireGuard Service at its new gateway is lost. A link whose round trip exceeds the health
// check's timeout of 500ms is never healthy, so a handshake's answer comes well within StaleRetry.
const StaleRetry = time.Second

// SessionInterval is how often the agent reads the counters of its WireGuard peers.
const SessionInterval = 250 * time.Millisecond

// session is what the agent last saw of a dialing link's peer.
type session struct {
	peer   wgtypes.Key
	rx, tx int64
	// heard is when rx last changed, or when the agent last dropped the session.
	heard time.Time
	// sent is true when tx changed since heard.
	sent bool
	// stall is when the peer last sent something before the first drop of the current stall. Zero outside a stall.
	stall time.Time
	// drops counts the drops of the current stall.
	drops int
}

// stall is a dialing link whose session the agent drops, or whose peer answers again after a drop.
type stall struct {
	W Interface
	// For drops: how long the peer has been silent. For answers: how long the stall lasted.
	Silent time.Duration
	// For drops: the drops of the stall before this one. For answers: every drop of the stall.
	Drops int
}

// sessions tracks the peer of each dialing link, from one leadership to its end.
type sessions map[string]*session

// check compares the peers' counters with the last read. It returns the dialing links whose peer sent nothing for
// StaleSession while this side sent, or for StaleRetry since the last drop, and those whose peer answers again after a
// drop. A link with no resolved endpoint waits for its peer to dial, and is never dropped.
func (s sessions) check(want []Interface, devs []Device, now time.Time) (drop, answered []stall) {
	byName := map[string]Device{}
	for _, d := range devs {
		byName[d.Name] = d
	}
	seen := map[string]bool{}
	for _, w := range want {
		d, ok := byName[w.Name]
		if w.Endpoint == nil || !ok || !d.Up {
			continue
		}
		var p *wgtypes.Peer
		for i := range d.Peers {
			if d.Peers[i].PublicKey == w.Peer {
				p = &d.Peers[i]
			}
		}
		if p == nil {
			continue
		}
		seen[w.Name] = true
		st := s[w.Name]
		if st == nil || st.peer != w.Peer {
			s[w.Name] = &session{peer: w.Peer, rx: p.ReceiveBytes, tx: p.TransmitBytes, heard: now}
			continue
		}
		switch {
		case p.ReceiveBytes != st.rx:
			if st.drops > 0 {
				answered = append(answered, stall{W: w, Silent: now.Sub(st.stall), Drops: st.drops})
			}
			st.heard, st.sent, st.stall, st.drops = now, false, time.Time{}, 0
		case p.TransmitBytes != st.tx:
			st.sent = true
		}
		st.rx, st.tx = p.ReceiveBytes, p.TransmitBytes
		wait := StaleSession
		if st.drops > 0 {
			wait = StaleRetry
		}
		if st.sent && now.Sub(st.heard) >= wait {
			since := st.stall
			if st.drops == 0 {
				since = st.heard
			}
			drop = append(drop, stall{W: w, Silent: now.Sub(since), Drops: st.drops})
		}
	}
	for name := range s {
		if !seen[name] {
			delete(s, name)
		}
	}
	return drop, answered
}

// dropped records that the agent dropped a link's session. The new peer's counters start at zero, and the drop counts as
// heard, so a peer that stays silent is dropped again every StaleRetry.
func (s sessions) dropped(name string, now time.Time) {
	st := s[name]
	if st == nil {
		return
	}
	if st.drops == 0 {
		st.stall = st.heard
	}
	st.drops++
	st.rx, st.tx, st.heard, st.sent = 0, 0, now, false
}

// renewConfig replaces the link's peer with its wanted settings in one change. The kernel drops the peer's session, and
// the next packet to the peer starts a handshake at once.
func renewConfig(w Interface) wgtypes.Config {
	ka := w.Keepalive
	return wgtypes.Config{
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   w.Peer,
			Endpoint:                    w.Endpoint,
			PersistentKeepaliveInterval: &ka,
			ReplaceAllowedIPs:           true,
			AllowedIPs:                  []net.IPNet{ipNet(w.PeerIP)},
		}},
	}
}

// Poke sends one empty UDP datagram to a peer's tunnel address, on the discard port. WireGuard holds it and sends a
// handshake at once, rather than at the next packet that a health check sends. The peer drops the datagram.
func Poke(peer netip.Addr) error {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer, 9)))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Write(nil)
	return err
}
