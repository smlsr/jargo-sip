package sip

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/g711"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func pcmuMedia(ip string, rtpPort, rtcpPort int) RemoteMedia {
	return RemoteMedia{
		Connection: net.ParseIP(ip),
		RTPPort:    rtpPort,
		RTCPPort:   rtcpPort,
		Audio:      Format{Payload: 0, Name: string(CodecPCMU), Clock: wireClock},
		DTMF:       &Format{Payload: 101, Name: telephoneEvent, Clock: wireClock, FMTP: "0-15"},
	}
}

func listenLoop(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func readUDP(t *testing.T, c *net.UDPConn, d time.Duration) ([]byte, *net.UDPAddr) {
	t.Helper()
	buf := make([]byte, 2048)
	_ = c.SetReadDeadline(time.Now().Add(d))
	n, addr, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf[:n]...), addr
}

func TestSilenceGoesToSDPTarget(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rtpSock := listenLoop(t)
	rtcpSock := listenLoop(t)
	m, err := StartMedia(context.Background(), pair, pcmuMedia("127.0.0.1", rtpSock.LocalAddr().(*net.UDPAddr).Port, rtcpSock.LocalAddr().(*net.UDPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if pair.LocalRTCPPort != pair.LocalRTPPort+1 {
		t.Fatalf("RTCP port %d RTP port %d", pair.LocalRTCPPort, pair.LocalRTPPort)
	}

	raw, from := readUDP(t, rtpSock, time.Second)
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if !pkt.Marker || pkt.PayloadType != 0 || pkt.Version != 2 || len(pkt.Payload) != frameSamples {
		t.Fatalf("first packet marker %v pt %d len %d", pkt.Marker, pkt.PayloadType, len(pkt.Payload))
	}
	if !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatal("first payload was not µ-law silence")
	}
	if from.Port != pair.LocalRTPPort {
		t.Fatalf("source port %d", from.Port)
	}
	if m.RTPLatch() != LatchSDPTarget {
		t.Fatalf("latch %s before any peer packet", m.RTPLatch())
	}
	if m.RTPDest().Port != rtpSock.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("dest %s", m.RTPDest())
	}

	second, _ := readUDP(t, rtpSock, time.Second)
	nxt := &rtp.Packet{}
	if err := nxt.Unmarshal(second); err != nil {
		t.Fatal(err)
	}
	if nxt.Marker {
		t.Fatal("second silence packet kept the marker")
	}
	if nxt.SequenceNumber != pkt.SequenceNumber+1 {
		t.Fatalf("sequence %d then %d", pkt.SequenceNumber, nxt.SequenceNumber)
	}
	if nxt.Timestamp != pkt.Timestamp+frameSamples || nxt.SSRC != pkt.SSRC {
		t.Fatalf("clock ts %d -> %d ssrc %d %d", pkt.Timestamp, nxt.Timestamp, pkt.SSRC, nxt.SSRC)
	}
}

func TestLatchFromNonSDPPort(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blackhole := listenLoop(t)
	m, err := StartMedia(context.Background(), pair, pcmuMedia("127.0.0.1", blackhole.LocalAddr().(*net.UDPAddr).Port, blackhole.LocalAddr().(*net.UDPAddr).Port+1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	readUDP(t, blackhole, time.Second)

	peer := listenLoop(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr)
	mediaAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pair.LocalRTPPort}
	for i := 0; i < 3; i++ {
		raw := mustPkt(t, uint16(100+i), uint32(i)*160, 42, 0, audio(160))
		if _, err := peer.WriteToUDP(raw, mediaAddr); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 2*time.Second, func() bool {
		return m.RTPLatch() == LatchCommitted && m.RTPDest().Port == peerAddr.Port
	})

	saw := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !saw {
		buf := make([]byte, 2048)
		_ = peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		pkt := &rtp.Packet{}
		if pkt.Unmarshal(buf[:n]) == nil && bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
			saw = true
		}
	}
	if !saw {
		t.Fatal("sender did not retarget to the learned port")
	}

	spoof := listenLoop(t)
	for i := 0; i < 5; i++ {
		raw := mustPkt(t, uint16(i), uint32(i)*160, 99, 0, audio(160))
		if _, err := spoof.WriteToUDP(raw, mediaAddr); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool { return m.DroppedRTP() >= 1 })
	if m.RTPDest().Port != peerAddr.Port || m.RTPLatch() != LatchCommitted {
		t.Fatalf("spoof moved latch to %s %s", m.RTPLatch(), m.RTPDest())
	}
	if m.DroppedRTP() < 1 {
		t.Fatal("spoof was not counted")
	}
}

func TestTwoLocalSessionsRTCP(t *testing.T) {
	cfg := baseConfig()
	cfg.RTCP.Interval = 20 * time.Millisecond
	ep := startEP(t, cfg)
	ctx := context.Background()
	a, err := ep.AllocateRTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ep.AllocateRTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.LocalRTCPPort != a.LocalRTPPort+1 || b.LocalRTCPPort != b.LocalRTPPort+1 {
		t.Fatal("pairs are not consecutive")
	}
	ma, err := StartMedia(ctx, a, pcmuMedia("127.0.0.1", b.LocalRTPPort, b.LocalRTCPPort))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := StartMedia(ctx, b, pcmuMedia("127.0.0.1", a.LocalRTPPort, a.LocalRTCPPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ma.Close()
		_ = mb.Close()
	})
	waitFor(t, 2*time.Second, func() bool {
		return ma.RTPLatch() == LatchCommitted && mb.RTPLatch() == LatchCommitted &&
			ma.ReceivedSenderReports() > 0 && mb.ReceivedSenderReports() > 0
	})
	if ma.RTCPDest().Port != b.LocalRTCPPort || mb.RTCPDest().Port != a.LocalRTCPPort {
		t.Fatalf("rtcp dest %s %s", ma.RTCPDest(), mb.RTCPDest())
	}
	if ma.Stats().SSRC == 0 || ma.Stats().SSRC == mb.Stats().SSRC {
		t.Fatalf("ssrc %d %d", ma.Stats().SSRC, mb.Stats().SSRC)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, okA := ma.RoundTrip()
		_, okB := mb.RoundTrip()
		return okA && okB
	})

	_ = ma.Close()
	waitFor(t, time.Second, func() bool { return mb.RTCPByeCount() > 0 })
	if mb.RTPLatch() != LatchCommitted {
		t.Fatal("RTCP BYE hung up the RTP latch")
	}
}

func TestRTCPOnPairedPortAndBYE(t *testing.T) {
	cfg := baseConfig()
	cfg.RTCP.Interval = 20 * time.Millisecond
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rtpSock := listenLoop(t)
	rtcpSock := listenLoop(t)
	remote := pcmuMedia("127.0.0.1", rtpSock.LocalAddr().(*net.UDPAddr).Port, 0)
	remote.RTCPExplicit = true
	remote.RTCPPort = rtcpSock.LocalAddr().(*net.UDPAddr).Port
	m, err := StartMedia(context.Background(), pair, remote)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	raw, _ := readUDP(t, rtcpSock, time.Second)
	pkts, err := rtcp.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	sr, ok := pkts[0].(*rtcp.SenderReport)
	if !ok {
		t.Fatalf("first RTCP packet %T", pkts[0])
	}
	if sr.SSRC != m.Stats().SSRC || sr.PacketCount < 1 || sr.OctetCount < frameSamples {
		t.Fatalf("sr %+v", sr)
	}
	var cname string
	for _, pkt := range pkts {
		sdes, ok := pkt.(*rtcp.SourceDescription)
		if !ok {
			continue
		}
		for _, chunk := range sdes.Chunks {
			for _, item := range chunk.Items {
				if item.Type == rtcp.SDESCNAME {
					cname = item.Text
				}
			}
		}
	}
	if cname != "bot@sip.example.com" {
		t.Fatalf("cname %q", cname)
	}

	// RTP on the RTCP socket is logged once and does not move the RTCP latch.
	if _, err := rtpSock.WriteToUDP(mustPkt(t, 1, 160, 5, 0, audio(160)), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pair.LocalRTCPPort}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, m.RTPOnRTCP)
	if m.RTCPLatch() != LatchSDPTarget {
		t.Fatalf("RTP moved the RTCP latch to %s", m.RTCPLatch())
	}

	bye, err := rtcp.Marshal([]rtcp.Packet{
		&rtcp.ReceiverReport{SSRC: 77},
		&rtcp.Goodbye{Sources: []uint32{77}},
		rtcp.NewCNAMESourceDescription(77, "peer@example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}
	peer := listenLoop(t)
	if _, err := peer.WriteToUDP(bye, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pair.LocalRTCPPort}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return m.RTCPByeCount() > 0 && m.RTCPLatch() == LatchProvisional })
	if _, err := peer.WriteToUDP(bye, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pair.LocalRTCPPort}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return m.RTCPLatch() == LatchCommitted && m.RTCPDest().Port == peer.LocalAddr().(*net.UDPAddr).Port
	})
}

func TestWirePCMMatchesG711(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sock := listenLoop(t)
	m, err := StartMedia(context.Background(), pair, pcmuMedia("127.0.0.1", sock.LocalAddr().(*net.UDPAddr).Port, sock.LocalAddr().(*net.UDPAddr).Port+1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	pcm := make([]byte, frameBytes)
	for i := 0; i < len(pcm); i += 2 {
		sample := int16(1000 - i)
		pcm[i] = byte(sample)
		pcm[i+1] = byte(sample >> 8)
	}
	if err := m.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	want := g711.EncodeULaw(pcm)
	var prev *rtp.Packet
	saw := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !saw {
		raw, _ := readUDP(t, sock, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if prev != nil {
			if pkt.Timestamp != prev.Timestamp+frameSamples || pkt.SequenceNumber != prev.SequenceNumber+1 {
				t.Fatalf("step seq %d->%d ts %d->%d", prev.SequenceNumber, pkt.SequenceNumber, prev.Timestamp, pkt.Timestamp)
			}
		}
		prev = pkt
		if bytes.Equal(pkt.Payload, want) {
			if !pkt.Marker {
				t.Fatal("first audio packet after silence had no marker")
			}
			saw = true
		}
	}
	if !saw {
		t.Fatal("wire payload did not match EncodeULaw")
	}

	before := m.Stats()
	if err := m.SetDirection(DirectionSendOnly); err != nil {
		t.Fatal(err)
	}
	if err := m.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	held := false
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !held {
		raw, _ := readUDP(t, sock, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if pkt.SequenceNumber == before.LastSequence {
			continue
		}
		if bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) && pkt.Marker {
			if pkt.SequenceNumber <= before.LastSequence && before.Packets > 0 {
				t.Fatalf("sequence went backwards %d -> %d", before.LastSequence, pkt.SequenceNumber)
			}
			held = true
		}
	}
	if !held {
		t.Fatal("sendonly did not send marked silence")
	}
}

func TestRetargetKeepsSSRC(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := listenLoop(t)
	m, err := StartMedia(context.Background(), pair, pcmuMedia("127.0.0.1", first.LocalAddr().(*net.UDPAddr).Port, first.LocalAddr().(*net.UDPAddr).Port+1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	raw, _ := readUDP(t, first, time.Second)
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	next := listenLoop(t)
	if err := m.Retarget(pcmuMedia("127.0.0.1", next.LocalAddr().(*net.UDPAddr).Port, next.LocalAddr().(*net.UDPAddr).Port+1)); err != nil {
		t.Fatal(err)
	}
	if m.RTPLatch() != LatchSDPTarget {
		t.Fatalf("latch %s", m.RTPLatch())
	}
	again, _ := readUDP(t, next, time.Second)
	got := &rtp.Packet{}
	if err := got.Unmarshal(again); err != nil {
		t.Fatal(err)
	}
	if got.SSRC != pkt.SSRC {
		t.Fatalf("ssrc %d -> %d", pkt.SSRC, got.SSRC)
	}
	delta := int(got.SequenceNumber - pkt.SequenceNumber)
	if delta < 1 || delta > 20 {
		t.Fatalf("sequence jump %d", delta)
	}
	if got.Timestamp != pkt.Timestamp+uint32(delta)*frameSamples {
		t.Fatalf("timestamp jump %d over %d packets", got.Timestamp-pkt.Timestamp, delta)
	}
}

func TestClockMustStay8000(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remote := pcmuMedia("127.0.0.1", 10000, 10001)
	remote.Audio.Clock = 16000
	if _, err := StartMedia(context.Background(), pair, remote); err == nil {
		t.Fatal("16000 Hz session was accepted")
	}
}
