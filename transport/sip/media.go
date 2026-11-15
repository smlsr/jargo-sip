package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// RemoteMedia is the SDP target a session sends to until its latch moves.
type RemoteMedia struct {
	Connection   net.IP
	RTPPort      int
	RTCPPort     int
	RTCPExplicit bool
	RTCPIP       net.IP
	Audio        Format
	DTMF         *Format
}

// Media is one RTP session on an RTPPair. RTP and RTCP each have a latch.
// The sender starts with codec silence toward the SDP address.
type Media struct {
	pair *RTPPair
	ep   *Endpoint
	rtp  *net.UDPConn
	rtcp *net.UDPConn

	rtpLatch  *rtpLatch
	rtcpLatch *rtcpLatch
	send      *sender
	recv      *reception
	play      *jitter
	pipe      *Pipeline

	interval time.Duration
	cname    string
	tones    *dtmfRecv

	stop chan struct{}
	kick chan struct{}
	wg   sync.WaitGroup

	mu         sync.Mutex
	stopped    bool
	playout    bool
	digitDur   time.Duration
	dtmfPT     int
	dtmfFMTP   string
	sdpRTP     *net.UDPAddr
	sdpRTCP    *net.UDPAddr
	byeCount   int
	rtpOnRTCP  bool
	srIn       int
	rtt        time.Duration
	haveRTT    bool
	lastReport time.Time
	srMarks    []srMark
}

type srMark struct {
	lsr uint32
	at  time.Time
}

// StartMedia sends silence to the SDP target and starts the RTP and RTCP latches.
// The RTCP socket is the pair's port, which is the RTP port plus one.
func StartMedia(ctx context.Context, pair *RTPPair, remote RemoteMedia) (*Media, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pair == nil || pair.endpoint == nil || pair.RTP == nil || pair.RTCP == nil {
		return nil, errors.New("rtp pair is missing")
	}
	rtpAddr, rtcpAddr, err := remote.targets()
	if err != nil {
		return nil, err
	}
	params, err := pair.endpoint.mediaParams()
	if err != nil {
		return nil, err
	}
	snd, err := newSender(remote.Audio)
	if err != nil {
		return nil, err
	}
	digit := params.digit
	if digit <= 0 {
		digit = 160 * time.Millisecond
	}
	m := &Media{
		pair:      pair,
		ep:        pair.endpoint,
		rtp:       pair.RTP,
		rtcp:      pair.RTCP,
		rtpLatch:  newRTPLatch(params.match, params.allow, rtpAddr, uint8(remote.Audio.Payload), eventPayload(remote.DTMF, remote.Audio.Payload)),
		rtcpLatch: newRTCPLatch(params.match, params.allow, rtcpAddr),
		send:      snd,
		recv:      &reception{},
		play:      newJitter(params.jitter, Codec(remote.Audio.Name)),
		tones:     newDTMFRecv(),
		interval:  params.interval,
		cname:     params.cname,
		digitDur:  digit,
		stop:      make(chan struct{}),
		kick:      make(chan struct{}, 1),
		playout:   true,
		sdpRTP:    cloneUDP(rtpAddr),
		sdpRTCP:   cloneUDP(rtcpAddr),
	}
	m.send.setGap(gapTicks(params.gap))
	m.bindDTMF(remote)
	if err := m.sendOne(); err != nil {
		return nil, err
	}
	m.wg.Add(6)
	go m.sendLoop()
	go m.playLoop()
	go m.readRTP()
	go m.readRTCP()
	go m.rtcpLoop()
	go m.latchLoop()
	go m.watch(ctx)
	if err := pair.endpoint.attachMedia(pair, m); err != nil {
		m.halt()
		m.wg.Wait()
		return nil, err
	}
	return m, nil
}

// Close stops the sender and the readers and sends an RTCP BYE.
// The RTP sockets stay open until the pair is closed.
func (m *Media) Close() error {
	if m == nil {
		return nil
	}
	m.halt()
	m.wg.Wait()
	if m.ep != nil {
		m.ep.detachMedia(m)
	}
	return nil
}

// ReadPCM returns one 20 ms frame of 8000 Hz 16-bit little-endian PCM.
// It blocks until that frame is due. A gap returns the previous frame and a
// nil error. Stopped playout returns concealment instead of caller audio.
// After Close it returns ErrMediaClosed.
func (m *Media) ReadPCM() ([]byte, error) {
	if m == nil || m.play == nil {
		return nil, ErrMediaClosed
	}
	m.mu.Lock()
	stopped := m.stopped
	on := m.playout
	m.mu.Unlock()
	if stopped {
		return nil, ErrMediaClosed
	}
	if !on {
		return concealmentPCM(), nil
	}
	return m.play.read()
}

// SetPlayout stops or restores caller audio. Stopping does not change the
// jitter buffer. ReadPCM returns concealment until playout is restored.
func (m *Media) SetPlayout(on bool) error {
	if err := m.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	m.playout = on
	m.mu.Unlock()
	return nil
}

// WritePCM queues 8000 Hz 16-bit little-endian PCM. The sender pulls 20 ms.
func (m *Media) WritePCM(pcm []byte) error {
	if err := m.alive(); err != nil {
		return err
	}
	return m.send.writePCM(pcm)
}

// flushPCM puts every full frame already queued onto the RTP socket. The
// 20 ms loop is about to stop, and a frame still queued would not go out.
func (m *Media) flushPCM() {
	if m == nil || m.send == nil || m.rtp == nil {
		return
	}
	m.mu.Lock()
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		return
	}
	dest := m.rtpLatch.dest()
	if dest == nil {
		return
	}
	for {
		raw, err := m.send.takeQueuedPCM()
		if err != nil || len(raw) == 0 {
			return
		}
		if _, err := m.rtp.WriteToUDP(raw, dest); err != nil {
			return
		}
	}
}

// ReadDTMF returns one event per press. The first end bit for
// (SSRC, timestamp, event) emits it. A new timestamp emits the previous
// press when that press never set the end bit. Retransmitted end packets
// do not emit again. A-D and flash are returned here. After Close it
// returns ErrMediaClosed.
func (m *Media) ReadDTMF() (DTMF, error) {
	if m == nil || m.tones == nil {
		return DTMF{}, ErrMediaClosed
	}
	return m.tones.read()
}

// WriteDTMF queues a telephone-event after PCM already waiting.
// The event replaces audio for the press. The timestamp stays at the start.
// Every 20 ms the sequence increments and the duration grows by 160.
// Three end packets follow. Duration zero uses the configured digit duration.
// A non-zero duration rounds up to 20 ms. ErrDTMFNotNegotiated sends nothing.
func (m *Media) WriteDTMF(ev DTMF) error {
	return m.queueDTMF(ev, false)
}

// WriteDTMFUrgent queues the event ahead of PCM so the next 20 ms tick sends it.
// The packet shape matches WriteDTMF.
func (m *Media) WriteDTMFUrgent(ev DTMF) error {
	return m.queueDTMF(ev, true)
}

// UsePipeline attaches the resample edge. Clear runs with the jitter reset
// when the remote address changes. A nil pipeline leaves PCM at 8000 Hz.
func (m *Media) UsePipeline(p *Pipeline) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrMediaClosed
	}
	m.pipe = p
	return nil
}

// SetDirection forces codec silence for sendonly, recvonly, and inactive.
// sendrecv lets queued PCM onto the wire again. Sequence and timestamp stay.
func (m *Media) SetDirection(d Direction) error {
	if err := m.alive(); err != nil {
		return err
	}
	return m.send.setDirection(d)
}

// Retarget points the session at a new SDP address.
// A changed connection or RTP port returns both latches to SDPTarget.
// SSRC, sequence, timestamp, and the local sockets stay.
func (m *Media) Retarget(remote RemoteMedia) error {
	if err := m.alive(); err != nil {
		return err
	}
	rtpAddr, rtcpAddr, err := remote.targets()
	if err != nil {
		return err
	}
	if err := m.send.setCodec(remote.Audio); err != nil {
		return err
	}
	m.play.setCodec(Codec(remote.Audio.Name))
	m.rtpLatch.setPayloads(uint8(remote.Audio.Payload), eventPayload(remote.DTMF, remote.Audio.Payload))
	m.bindDTMF(remote)
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrMediaClosed
	}
	moved := !sameAddr(m.sdpRTP, rtpAddr)
	m.sdpRTP = cloneUDP(rtpAddr)
	m.sdpRTCP = cloneUDP(rtcpAddr)
	m.mu.Unlock()
	if moved {
		m.rtpLatch.retarget(rtpAddr)
		m.rtcpLatch.retarget(rtcpAddr)
		m.recv.reset()
		m.play.reset()
		m.clearPipe()
		return nil
	}
	m.rtcpLatch.retargetIfUnmoved(rtcpAddr)
	return nil
}

// RTPLatch returns the RTP latch state.
func (m *Media) RTPLatch() LatchState { return m.rtpLatch.snapshot() }

// RTCPLatch returns the RTCP latch state.
func (m *Media) RTCPLatch() LatchState { return m.rtcpLatch.snapshot() }

// RTPDest is the address the next RTP packet is written to.
func (m *Media) RTPDest() *net.UDPAddr { return m.rtpLatch.dest() }

// RTCPDest is the address the next RTCP packet is written to.
func (m *Media) RTCPDest() *net.UDPAddr { return m.rtcpLatch.dest() }

// Stats returns the local SSRC and the last packet the sender transmitted.
func (m *Media) Stats() SenderStats { return m.send.stats() }

// DroppedRTP is how many plausible packets the RTP latch refused.
func (m *Media) DroppedRTP() int { return m.rtpLatch.droppedCount() }

// RTCPByeCount is how many RTCP BYE packets this session has seen.
func (m *Media) RTCPByeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byeCount
}

// ReceivedSenderReports is how many RTCP sender reports arrived on the RTCP socket.
func (m *Media) ReceivedSenderReports() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.srIn
}

// RTPOnRTCP reports whether an RTP packet was seen on the RTCP socket.
func (m *Media) RTPOnRTCP() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rtpOnRTCP
}

// RoundTrip is the last RTT computed from LSR and DLSR. It is not applied
// to a playout target.
func (m *Media) RoundTrip() (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rtt, m.haveRTT
}

func (m *Media) alive() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrMediaClosed
	}
	return nil
}

func (m *Media) halt() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	m.mu.Unlock()
	m.send.markClosed()
	if m.tones != nil {
		m.tones.shutdown()
	}
	close(m.stop)
	_ = m.rtp.SetReadDeadline(time.Unix(1, 0))
	_ = m.rtcp.SetReadDeadline(time.Unix(1, 0))
}

func (m *Media) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = m.Close()
	case <-m.stop:
	}
}

func (m *Media) sendLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			if err := m.sendOne(); err != nil && (errors.Is(err, net.ErrClosed) || errors.Is(err, ErrMediaClosed)) {
				return
			}
		}
	}
}

func (m *Media) sendOne() error {
	dest := m.rtpLatch.dest()
	if dest == nil {
		return nil
	}
	raw, err := m.send.packet()
	if err != nil {
		return err
	}
	_, err = m.rtp.WriteToUDP(raw, dest)
	return err
}

func (m *Media) readRTP() {
	defer m.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, err := m.rtp.ReadFromUDP(buf)
		if err != nil {
			if m.readDone(err, m.rtp) {
				return
			}
			continue
		}
		m.takeRTP(buf[:n], addr, time.Now())
	}
}

func (m *Media) takeRTP(b []byte, addr *net.UDPAddr, now time.Time) {
	before := m.rtpLatch.dest()
	acc := m.rtpLatch.observe(b, addr, now)
	if !acc.ok {
		return
	}
	if !sameAddr(before, m.rtpLatch.dest()) {
		// The first packet moves SDPTarget to the peer. The ring is empty
		// then, so reception state from an earlier RTCP report stays.
		// A later latch switch discards audio that belonged to the old address.
		if m.play.reset() {
			m.recv.reset()
		}
		m.clearPipe()
	}
	m.ingest(acc, now)
}

func (m *Media) ingest(acc rtpAccept, now time.Time) {
	if acc.audio {
		// Audio has its own RTP timestamp. A newer timestamp completes an
		// open press when the end bit never arrived. An older one does not.
		if m.tones != nil {
			m.tones.noteTimestamp(acc.ssrc, acc.ts)
		}
		pcm, err := decodePayload(m.send.codecName(), acc.payload)
		if err != nil || (len(pcm) != frameBytes && len(pcm) != frameBytes/2) {
			m.play.count(acc.ssrc, acc.seq)
		} else {
			m.play.pushAudio(acc.ssrc, acc.seq, pcm, now)
		}
	} else {
		// Telephone-event is not G.711. The sequence is consumed so the
		// playout tick conceals that 20 ms instead of counting a loss.
		m.play.pushEvent(acc.ssrc, acc.seq, now)
		if m.tones != nil {
			m.tones.observe(acc.ssrc, acc.ts, acc.payload)
		}
	}
	m.recv.note(acc.ssrc, uint32(acc.seq), acc.ts, now, acc.audio)
}

func (m *Media) bindDTMF(remote RemoteMedia) {
	pt := eventPayload(remote.DTMF, remote.Audio.Payload)
	fmtp := "0-15"
	if remote.DTMF != nil && remote.DTMF.FMTP != "" {
		fmtp = remote.DTMF.FMTP
	}
	m.mu.Lock()
	m.dtmfPT = pt
	m.dtmfFMTP = fmtp
	m.mu.Unlock()
	if m.tones != nil {
		m.tones.setSpec(pt >= 0, fmtp)
	}
}

func (m *Media) playLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			m.play.shutdown()
			return
		case now := <-ticker.C:
			m.play.step(now)
		}
	}
}

func (m *Media) clearPipe() {
	m.mu.Lock()
	p := m.pipe
	m.mu.Unlock()
	if p != nil {
		p.Clear()
	}
}

func (m *Media) latchLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.rtpLatch.advance(now)
			m.rtcpLatch.advance(now)
		}
	}
}

func (m *Media) readDone(err error, conn *net.UDPConn) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		select {
		case <-m.stop:
			return true
		default:
		}
		_ = conn.SetReadDeadline(time.Time{})
		return false
	}
	return true
}

func (m *Media) watchRequest() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (remote RemoteMedia) targets() (*net.UDPAddr, *net.UDPAddr, error) {
	ip, err := ipv4(remote.Connection)
	if err != nil {
		return nil, nil, err
	}
	if remote.RTPPort < 1 || remote.RTPPort > 65535 {
		return nil, nil, fmt.Errorf("RTP port %d is out of range", remote.RTPPort)
	}
	if err := checkAudio(remote.Audio); err != nil {
		return nil, nil, err
	}
	var rtcpIP net.IP
	rtcpPort := remote.RTPPort + 1
	if remote.RTCPExplicit {
		if remote.RTCPPort < 1 || remote.RTCPPort > 65535 {
			return nil, nil, fmt.Errorf("RTCP port %d is out of range", remote.RTCPPort)
		}
		if remote.RTCPPort == remote.RTPPort {
			return nil, nil, errors.New("RTCP port equals the RTP port")
		}
		rtcpPort = remote.RTCPPort
		rtcpIP = ip
		if len(remote.RTCPIP) > 0 {
			rtcpIP, err = ipv4(remote.RTCPIP)
			if err != nil {
				return nil, nil, err
			}
		}
	} else if remote.RTPPort == 65535 {
		return nil, nil, errors.New("RTCP port is out of range")
	} else {
		rtcpIP = ip
	}
	return &net.UDPAddr{IP: cloneIP(ip), Port: remote.RTPPort},
		&net.UDPAddr{IP: cloneIP(rtcpIP), Port: rtcpPort}, nil
}

type mediaParams struct {
	match    string
	allow    []net.IP
	interval time.Duration
	cname    string
	jitter   JitterConfig
	digit    time.Duration
	gap      time.Duration
}

func (e *Endpoint) mediaParams() (mediaParams, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.started {
		return mediaParams{}, errors.New("endpoint is not started")
	}
	allow, err := parseAllowRTP(e.cfg.Latch.AllowRTPFrom)
	if err != nil {
		return mediaParams{}, err
	}
	interval := e.cfg.RTCP.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return mediaParams{
		match:    e.cfg.Latch.Match,
		allow:    allow,
		interval: interval,
		cname:    e.cfg.User + "@" + e.cfg.Domain,
		jitter:   e.cfg.Jitter,
		digit:    e.cfg.DTMF.DigitDuration,
		gap:      e.cfg.DTMF.InterDigitGap,
	}, nil
}

func (e *Endpoint) attachMedia(p *RTPPair, m *Media) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.started || p.released || p.endpoint != e {
		return errors.New("rtp pair is closed")
	}
	p.media = m
	e.medias = append(e.medias, m)
	return nil
}

func (e *Endpoint) detachMedia(m *Media) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, q := range e.medias {
		if q == m {
			continue
		}
		e.medias[n] = q
		n++
	}
	for i := n; i < len(e.medias); i++ {
		e.medias[i] = nil
	}
	e.medias = e.medias[:n]
	if m.pair != nil && m.pair.media == m {
		m.pair.media = nil
	}
}
