package sip

import (
	"encoding/binary"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	stunCookie  = 0x2112A442
	latchQuiet  = 100 * time.Millisecond
	latchWindow = 500 * time.Millisecond
	rtpCommitN  = 3
	rtcpCommitN = 2
)

// LatchState is how far a media latch has moved from the SDP address.
type LatchState uint8

const (
	// LatchSDPTarget sends to the address taken from SDP.
	LatchSDPTarget LatchState = iota
	// LatchProvisional sends to the first accepted source.
	LatchProvisional
	// LatchCommitted freezes the address. Later sources are dropped.
	LatchCommitted
)

// String returns SDPTarget, Provisional, or Committed.
func (s LatchState) String() string {
	switch s {
	case LatchSDPTarget:
		return "SDPTarget"
	case LatchProvisional:
		return "Provisional"
	case LatchCommitted:
		return "Committed"
	default:
		return "unknown"
	}
}

type rtpAccept struct {
	ok    bool
	audio bool
	seq   uint16
	ts    uint32
	ssrc  uint32
}

type rtpSource struct {
	addr *net.UDPAddr
	n    int
}

// rtpLatch learns the RTP WriteTo address. It does not rebind the socket.
type rtpLatch struct {
	mu         sync.Mutex
	match      string
	sdpIP      net.IP
	allow      []net.IP
	sdp        *net.UDPAddr
	write      *net.UDPAddr
	state      LatchState
	audioPT    uint8
	dtmfPT     int
	ssrc       uint32
	haveSSRC   bool
	switched   bool
	first      time.Time
	since      time.Time
	contradict time.Time
	active     string
	commitN    int
	sources    map[string]*rtpSource
	dropped    int
	logged     map[string]struct{}
}

func newRTPLatch(match string, allow []net.IP, sdp *net.UDPAddr, audioPT uint8, dtmfPT int) *rtpLatch {
	return &rtpLatch{
		match:   match,
		sdpIP:   cloneIP(sdp.IP),
		allow:   cloneIPs(allow),
		sdp:     cloneUDP(sdp),
		write:   cloneUDP(sdp),
		state:   LatchSDPTarget,
		audioPT: audioPT,
		dtmfPT:  dtmfPT,
		sources: map[string]*rtpSource{},
	}
}

func (l *rtpLatch) dest() *net.UDPAddr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneUDP(l.write)
}

func (l *rtpLatch) snapshot() LatchState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *rtpLatch) droppedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

func (l *rtpLatch) setPayloads(audio uint8, dtmf int) {
	l.mu.Lock()
	l.audioPT = audio
	l.dtmfPT = dtmf
	l.mu.Unlock()
}

// retarget returns the latch to the SDP address. The local socket stays.
func (l *rtpLatch) retarget(sdp *net.UDPAddr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sdpIP = cloneIP(sdp.IP)
	l.sdp = cloneUDP(sdp)
	l.write = cloneUDP(sdp)
	l.state = LatchSDPTarget
	l.switched = false
	l.first = time.Time{}
	l.since = time.Time{}
	l.contradict = time.Time{}
	l.active = ""
	l.commitN = 0
	l.ssrc = 0
	l.haveSSRC = false
	l.sources = map[string]*rtpSource{}
	l.dropped = 0
	l.logged = nil
}

// observe applies one received datagram. A plausible packet that passes Match
// moves SDPTarget to Provisional and the sender uses that source on the next read.
func (l *rtpLatch) observe(b []byte, addr *net.UDPAddr, now time.Time) rtpAccept {
	l.mu.Lock()
	defer l.mu.Unlock()
	pkt, audio, ok := classifyRTP(b, l.audioPT, l.dtmfPT)
	if !ok || addr == nil {
		return rtpAccept{}
	}
	if !sourceAllowed(l.match, l.sdpIP, l.allow, addr) {
		return rtpAccept{}
	}
	key := addrKey(addr)
	// One SSRC for the session. A second SSRC is dropped before it is counted
	// or allowed to take the one provisional switch.
	if l.haveSSRC && pkt.SSRC != l.ssrc {
		l.dropped++
		l.logDrop(key, "ssrc")
		return rtpAccept{}
	}
	if l.state == LatchCommitted {
		return l.committed(key, pkt, audio)
	}
	src := l.sources[key]
	if src == nil {
		src = &rtpSource{addr: cloneUDP(addr)}
		l.sources[key] = src
	}
	src.n++
	if l.state == LatchSDPTarget {
		l.ssrc = pkt.SSRC
		l.haveSSRC = true
		l.become(key, src.addr, now)
		l.first = now
		return acceptOf(pkt, audio)
	}
	if key == l.active {
		l.commitN++
		if l.commitN >= rtpCommitN {
			l.state = LatchCommitted
		}
		return acceptOf(pkt, audio)
	}
	l.contradict = now
	// One switch, and only inside 500 ms of the first packet, to the source
	// that has strictly more plausible packets.
	active := l.sources[l.active]
	inside := !l.first.IsZero() && now.Sub(l.first) <= latchWindow
	if !l.switched && inside && active != nil && src.n > active.n {
		l.switched = true
		l.become(key, src.addr, now)
		return acceptOf(pkt, audio)
	}
	l.dropped++
	l.logDrop(key, "source")
	return rtpAccept{}
}

func (l *rtpLatch) committed(key string, pkt *rtp.Packet, audio bool) rtpAccept {
	if key != l.active {
		l.dropped++
		l.logDrop(key, "committed")
		return rtpAccept{}
	}
	return acceptOf(pkt, audio)
}

func (l *rtpLatch) become(key string, addr *net.UDPAddr, now time.Time) {
	l.state = LatchProvisional
	l.write = cloneUDP(addr)
	l.active = key
	l.commitN = 1
	l.since = now
	l.contradict = time.Time{}
	if l.commitN >= rtpCommitN {
		l.state = LatchCommitted
	}
}

func (l *rtpLatch) advance(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != LatchProvisional {
		return
	}
	if l.commitN >= rtpCommitN || quietDone(l.since, l.contradict, now) {
		l.state = LatchCommitted
	}
}

func (l *rtpLatch) logDrop(addr, why string) {
	if l.logged == nil {
		l.logged = map[string]struct{}{}
	}
	key := why + " " + addr
	if _, ok := l.logged[key]; ok {
		return
	}
	l.logged[key] = struct{}{}
	log.Printf("sip: RTP latch dropped %s (%s) in %s", addr, why, l.state)
}

// rtcpLatch learns the RTCP WriteTo address. It is independent of the RTP latch.
type rtcpLatch struct {
	mu         sync.Mutex
	match      string
	sdpIP      net.IP
	allow      []net.IP
	write      *net.UDPAddr
	state      LatchState
	since      time.Time
	contradict time.Time
	active     string
	commitN    int
	dropped    int
	logged     map[string]struct{}
}

func newRTCPLatch(match string, allow []net.IP, sdp *net.UDPAddr) *rtcpLatch {
	return &rtcpLatch{
		match: match,
		sdpIP: cloneIP(sdp.IP),
		allow: cloneIPs(allow),
		write: cloneUDP(sdp),
		state: LatchSDPTarget,
	}
}

func (l *rtcpLatch) dest() *net.UDPAddr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneUDP(l.write)
}

func (l *rtcpLatch) snapshot() LatchState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// retarget returns the latch to the SDP address.
func (l *rtcpLatch) retarget(sdp *net.UDPAddr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setSDP(sdp)
	l.dropped = 0
	l.logged = nil
}

// retargetIfUnmoved updates the SDP destination while the latch is still there.
func (l *rtcpLatch) retargetIfUnmoved(sdp *net.UDPAddr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != LatchSDPTarget {
		return
	}
	l.setSDP(sdp)
}

func (l *rtcpLatch) setSDP(sdp *net.UDPAddr) {
	l.sdpIP = cloneIP(sdp.IP)
	l.write = cloneUDP(sdp)
	l.state = LatchSDPTarget
	l.since = time.Time{}
	l.contradict = time.Time{}
	l.active = ""
	l.commitN = 0
}

// observe records one valid compound packet.
func (l *rtcpLatch) observe(addr *net.UDPAddr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if addr == nil || !sourceAllowed(l.match, l.sdpIP, l.allow, addr) {
		return
	}
	key := addrKey(addr)
	if l.state == LatchCommitted {
		if key != l.active {
			l.dropped++
			l.logDrop(key)
		}
		return
	}
	if l.state == LatchSDPTarget || key == l.active {
		if l.state == LatchSDPTarget {
			l.write = cloneUDP(addr)
			l.active = key
			l.commitN = 0
			l.since = now
			l.contradict = time.Time{}
			l.state = LatchProvisional
		}
		l.commitN++
		if l.commitN >= rtcpCommitN {
			l.state = LatchCommitted
		}
		return
	}
	// A contradicting source does not replace the provisional target.
	l.contradict = now
	l.dropped++
	l.logDrop(key)
}

func (l *rtcpLatch) advance(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != LatchProvisional {
		return
	}
	if l.commitN >= rtcpCommitN || quietDone(l.since, l.contradict, now) {
		l.state = LatchCommitted
	}
}

func (l *rtcpLatch) logDrop(addr string) {
	if l.logged == nil {
		l.logged = map[string]struct{}{}
	}
	if _, ok := l.logged[addr]; ok {
		return
	}
	l.logged[addr] = struct{}{}
	log.Printf("sip: RTCP latch dropped %s in %s", addr, l.state)
}

func quietDone(since, contradict, now time.Time) bool {
	if since.IsZero() || now.Sub(since) < latchQuiet {
		return false
	}
	if !contradict.IsZero() && !contradict.Before(since) && now.Sub(contradict) < latchQuiet {
		return false
	}
	return true
}

func sourceAllowed(match string, sdpIP net.IP, allow []net.IP, addr *net.UDPAddr) bool {
	if addr == nil || len(addr.IP) == 0 {
		return false
	}
	switch match {
	case LatchMatchOff:
		return false
	case LatchMatchAddress:
		return true
	case LatchMatchPort:
		if ipEqual(addr.IP, sdpIP) {
			return true
		}
		for _, ip := range allow {
			if ipEqual(addr.IP, ip) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func classifyRTP(b []byte, audioPT uint8, dtmfPT int) (*rtp.Packet, bool, bool) {
	if isSTUN(b) {
		return nil, false, false
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(b); err != nil {
		return nil, false, false
	}
	if pkt.Version != 2 {
		return nil, false, false
	}
	if pkt.PayloadType == audioPT {
		n := len(pkt.Payload)
		if n != frameSamples && n != frameSamples/2 {
			return nil, false, false
		}
		return pkt, true, true
	}
	if dtmfPT >= 0 && int(pkt.PayloadType) == dtmfPT {
		return pkt, false, true
	}
	return nil, false, false
}

func acceptOf(pkt *rtp.Packet, audio bool) rtpAccept {
	return rtpAccept{
		ok:    true,
		audio: audio,
		seq:   pkt.SequenceNumber,
		ts:    pkt.Timestamp,
		ssrc:  pkt.SSRC,
	}
}

func isSTUN(b []byte) bool {
	return len(b) >= 8 && binary.BigEndian.Uint32(b[4:8]) == stunCookie
}

func ipEqual(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	a4, b4 := a.To4(), b.To4()
	if a4 != nil && b4 != nil {
		return a4.Equal(b4)
	}
	return a.Equal(b)
}

func sameAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Port == b.Port && ipEqual(a.IP, b.IP)
}

func addrKey(a *net.UDPAddr) string {
	ip := a.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(a.Port))
}

func cloneUDP(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	return &net.UDPAddr{IP: cloneIP(a.IP), Port: a.Port, Zone: a.Zone}
}

func cloneIPs(in []net.IP) []net.IP {
	if len(in) == 0 {
		return nil
	}
	out := make([]net.IP, len(in))
	for i, ip := range in {
		out[i] = cloneIP(ip)
	}
	return out
}
