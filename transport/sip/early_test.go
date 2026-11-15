package sip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/g711"
	"github.com/pion/rtp"
)

func earlyBody(port int, telephone bool) []byte {
	formats := "0"
	attrs := "a=rtpmap:0 PCMU/8000\n"
	if telephone {
		formats = "0 101"
		attrs += "a=rtpmap:101 telephone-event/8000\na=fmtp:101 0-15\n"
	}
	return []byte(crlf(fmt.Sprintf(`v=0
o=- 9 1 IN IP4 127.0.0.1
s=-
c=IN IP4 127.0.0.1
t=0 0
m=audio %d RTP/AVP %s
%sa=ptime:20
a=sendrecv
`, port, formats, attrs)))
}

func offeredSDP(t *testing.T, ep *Endpoint, pair *RTPPair) Description {
	t.Helper()
	addrs, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	origin, err := NewOrigin()
	if err != nil {
		t.Fatal(err)
	}
	offered, _, err := origin.Offer(MediaAddr{IP: addrs.AdvertiseIP, RTPPort: pair.AdvertiseRTPPort}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	return offered
}

func TestEarlyMediaPlaysBefore200(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 20 * time.Millisecond
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	peer := listenLoop(t)
	port := peer.LocalAddr().(*net.UDPAddr).Port
	offered := offeredSDP(t, ep, pair)
	body := earlyBody(port, true)
	ctx := context.Background()

	idle, err := ApplyEarly(ctx, pair, nil, offered, nil)
	if err != nil || idle != nil {
		t.Fatalf("empty 183 media %v err %v", idle, err)
	}
	if pair.media != nil {
		t.Fatal("183 without SDP started media")
	}

	m, err := ApplyEarly(ctx, pair, nil, offered, body)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	raw, _ := readUDP(t, peer, time.Second)
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if pkt.PayloadType != 0 || !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatal("183 address was not sent codec silence")
	}
	ssrc := m.Stats().SSRC
	if m.RTPDest().Port != port {
		t.Fatalf("dest %s", m.RTPDest())
	}

	payloads := [][]byte{
		bytes.Repeat([]byte{0x21}, frameSamples),
		bytes.Repeat([]byte{0x22}, frameSamples),
		bytes.Repeat([]byte{0x23}, frameSamples),
	}
	for i, p := range payloads {
		injectAt(t, m, mustPkt(t, uint16(i+1), uint32(i+1)*160, 77, 0, p))
	}
	waitFor(t, time.Second, func() bool { return m.play.packets() >= 3 })
	learned := m.RTPDest().Port
	latch := m.RTPLatch()
	same, err := ConfirmAnswer(ctx, pair, m, offered, body)
	if err != nil {
		t.Fatal(err)
	}
	if same != m || m.Stats().SSRC != ssrc {
		t.Fatal("200 replaced the sender")
	}
	if m.play.packets() < 3 {
		t.Fatal("frames that arrived before 200 were discarded")
	}
	if m.RTPDest().Port != learned || m.RTPLatch() != latch {
		t.Fatalf("same address reset latch %s->%s dest %d->%d", latch, m.RTPLatch(), learned, m.RTPDest().Port)
	}
	for i, p := range payloads {
		frame := awaitPCM(t, m, time.Second)
		want := g711.DecodeULaw(p)
		if len(frame) != frameBytes || !bytes.Equal(frame, want) {
			t.Fatalf("frame %d len %d", i, len(frame))
		}
	}
}

func TestEarlyMediaWithoutTelephoneEventStillConnects(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	peer := listenLoop(t)
	offered := offeredSDP(t, ep, pair)
	m, err := ApplyEarly(context.Background(), pair, nil, offered, earlyBody(peer.LocalAddr().(*net.UDPAddr).Port, false))
	if err != nil || m == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	err = m.WriteDTMF(DTMF{Event: 5})
	if !errors.Is(err, ErrDTMFNotNegotiated) {
		t.Fatalf("err %v", err)
	}
	again, err := ApplyEarly(context.Background(), pair, m, offered, []byte(" \r\n"))
	if err != nil || again != m || again.RTPDest().Port != peer.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("empty 183 moved media %v %v", again, err)
	}
}

func TestConfirmMovesLatchAndKeepsSSRC(t *testing.T) {
	ep := startEP(t, baseConfig())
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := listenLoop(t)
	second := listenLoop(t)
	offered := offeredSDP(t, ep, pair)
	ctx := context.Background()
	m, err := ApplyEarly(ctx, pair, nil, offered, earlyBody(first.LocalAddr().(*net.UDPAddr).Port, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	ssrc := m.Stats().SSRC
	injectAt(t, m, mustPkt(t, 1, 160, 77, 0, bytes.Repeat([]byte{0x40}, frameSamples)))
	waitFor(t, time.Second, func() bool { return m.RTPLatch() != LatchSDPTarget && m.play.packets() > 0 })
	moved, err := ConfirmAnswer(ctx, pair, m, offered, earlyBody(second.LocalAddr().(*net.UDPAddr).Port, true))
	if err != nil {
		t.Fatal(err)
	}
	if moved != m || m.Stats().SSRC != ssrc {
		t.Fatal("200 with a new address replaced the sender")
	}
	if m.RTPLatch() != LatchSDPTarget || m.RTPDest().Port != second.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("latch %s dest %s", m.RTPLatch(), m.RTPDest())
	}
	if m.play.packets() != 0 {
		t.Fatal("new address kept the previous jitter buffer")
	}
	if _, err := ConfirmAnswer(ctx, pair, nil, offered, nil); err == nil {
		t.Fatal("200 without SDP was accepted")
	}
}
