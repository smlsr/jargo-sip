package sip

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func mustPkt(t *testing.T, seq uint16, ts, ssrc uint32, pt uint8, payload []byte) []byte {
	t.Helper()
	raw, err := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func audio(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = 0xFF
	}
	return p
}

func udp(ip string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
}

func TestRTPLatchThreePacketsAndSpoof(t *testing.T) {
	sdp := udp("10.0.0.1", 10000)
	l := newRTPLatch(LatchMatchPort, nil, sdp, 0, 101)
	now := time.Unix(1_700_000_000, 0)
	a := udp("10.0.0.1", 40000)
	for i := 0; i < 3; i++ {
		l.observe(mustPkt(t, uint16(i), uint32(i)*160, 7, 0, audio(160)), a, now.Add(time.Duration(i)*time.Millisecond))
	}
	if l.snapshot() != LatchCommitted {
		t.Fatalf("state %s", l.snapshot())
	}
	if l.dest().Port != 40000 {
		t.Fatalf("dest %s", l.dest())
	}
	spoof := udp("10.0.0.1", 40001)
	for i := 0; i < 5; i++ {
		got := l.observe(mustPkt(t, uint16(10+i), uint32(i)*160, 9, 0, audio(160)), spoof, now.Add(time.Second))
		if got.ok {
			t.Fatal("spoof was accepted")
		}
	}
	if l.dest().Port != 40000 || l.snapshot() != LatchCommitted {
		t.Fatalf("moved to %s %s", l.dest(), l.snapshot())
	}
	if l.droppedCount() < 5 {
		t.Fatalf("dropped %d", l.droppedCount())
	}
}

func TestRTPLatchQuietAndContradict(t *testing.T) {
	sdp := udp("10.0.0.1", 10000)
	l := newRTPLatch(LatchMatchAddress, nil, sdp, 0, -1)
	now := time.Unix(1_700_000_000, 0)
	a := udp("10.1.1.1", 40000)
	l.observe(mustPkt(t, 1, 160, 7, 0, audio(160)), a, now)
	if l.snapshot() != LatchProvisional {
		t.Fatalf("state %s", l.snapshot())
	}
	l.advance(now.Add(99 * time.Millisecond))
	if l.snapshot() != LatchProvisional {
		t.Fatal("committed early")
	}
	l.advance(now.Add(100 * time.Millisecond))
	if l.snapshot() != LatchCommitted || l.dest().Port != 40000 {
		t.Fatalf("quiet commit %s %s", l.snapshot(), l.dest())
	}

	l = newRTPLatch(LatchMatchAddress, nil, sdp, 0, -1)
	b := udp("10.2.2.2", 40001)
	l.observe(mustPkt(t, 1, 160, 7, 0, audio(160)), a, now)
	l.observe(mustPkt(t, 1, 160, 7, 0, audio(160)), b, now.Add(20*time.Millisecond))
	l.advance(now.Add(100 * time.Millisecond))
	if l.snapshot() == LatchCommitted {
		t.Fatal("contradicting source committed the latch")
	}
	l.advance(now.Add(120 * time.Millisecond))
	if l.snapshot() != LatchCommitted || l.dest().Port != 40000 {
		t.Fatalf("after quiet %s port %d", l.snapshot(), l.dest().Port)
	}
}

func TestRTPLatchSwitchesOnce(t *testing.T) {
	sdp := udp("10.0.0.1", 10000)
	l := newRTPLatch(LatchMatchAddress, nil, sdp, 0, -1)
	now := time.Unix(1_700_000_000, 0)
	a := udp("10.1.1.1", 40000)
	b := udp("10.2.2.2", 40001)
	c := udp("10.3.3.3", 40002)
	l.observe(mustPkt(t, 1, 160, 1, 0, audio(160)), a, now)
	l.observe(mustPkt(t, 1, 160, 2, 0, audio(160)), b, now.Add(10*time.Millisecond))
	if l.dest().Port != 40000 {
		t.Fatalf("switched early to %d", l.dest().Port)
	}
	l.observe(mustPkt(t, 2, 320, 2, 0, audio(160)), b, now.Add(20*time.Millisecond))
	if l.snapshot() != LatchProvisional || l.dest().Port != 40001 {
		t.Fatalf("switch %s %s", l.snapshot(), l.dest())
	}
	for i := 0; i < 5; i++ {
		l.observe(mustPkt(t, uint16(i+1), uint32(i)*160, 3, 0, audio(160)), c, now.Add(time.Duration(30+i)*time.Millisecond))
	}
	if l.dest().Port != 40001 {
		t.Fatalf("second switch to %d", l.dest().Port)
	}
	l.observe(mustPkt(t, 3, 480, 2, 0, audio(160)), b, now.Add(40*time.Millisecond))
	l.observe(mustPkt(t, 4, 640, 2, 0, audio(160)), b, now.Add(50*time.Millisecond))
	if l.snapshot() != LatchCommitted || l.dest().Port != 40001 {
		t.Fatalf("commit %s %s", l.snapshot(), l.dest())
	}

	late := newRTPLatch(LatchMatchAddress, nil, sdp, 0, -1)
	late.observe(mustPkt(t, 1, 160, 1, 0, audio(160)), a, now)
	late.observe(mustPkt(t, 1, 160, 2, 0, audio(160)), b, now.Add(501*time.Millisecond))
	late.observe(mustPkt(t, 2, 320, 2, 0, audio(160)), b, now.Add(520*time.Millisecond))
	if late.dest().Port != 40000 {
		t.Fatalf("switched after the window to %d", late.dest().Port)
	}
}

func TestRTPLatchMatch(t *testing.T) {
	sdp := udp("10.0.0.1", 10000)
	pkt := mustPkt(t, 1, 160, 7, 0, audio(160))
	now := time.Unix(1_700_000_000, 0)

	off := newRTPLatch(LatchMatchOff, nil, sdp, 0, -1)
	off.observe(pkt, udp("10.0.0.1", 40000), now)
	if off.snapshot() != LatchSDPTarget || off.dest().Port != 10000 {
		t.Fatalf("off moved to %s %s", off.snapshot(), off.dest())
	}

	port := newRTPLatch(LatchMatchPort, []net.IP{net.ParseIP("10.9.9.9")}, sdp, 0, -1)
	port.observe(pkt, udp("10.8.8.8", 40000), now)
	if port.snapshot() != LatchSDPTarget {
		t.Fatal("foreign IP latched in port mode")
	}
	port.observe(pkt, udp("10.9.9.9", 2222), now)
	if port.snapshot() != LatchProvisional || port.dest().Port != 2222 {
		t.Fatalf("allow list %s %s", port.snapshot(), port.dest())
	}

	addr := newRTPLatch(LatchMatchAddress, nil, sdp, 0, -1)
	addr.observe(pkt, udp("192.0.2.9", 7), now)
	if addr.snapshot() != LatchProvisional || addr.dest().Port != 7 {
		t.Fatalf("address mode %s %s", addr.snapshot(), addr.dest())
	}
}

func TestRTPLatchPlausible(t *testing.T) {
	sdp := udp("10.0.0.1", 10000)
	l := newRTPLatch(LatchMatchAddress, nil, sdp, 0, 101)
	now := time.Unix(1_700_000_000, 0)
	from := udp("10.0.0.1", 40000)
	stun := make([]byte, 20)
	stun[4], stun[5], stun[6], stun[7] = 0x21, 0x12, 0xA4, 0x42
	l.observe(stun, from, now)
	l.observe(mustPkt(t, 1, 160, 7, 0, audio(40)), from, now)
	l.observe(mustPkt(t, 1, 160, 7, 111, audio(160)), from, now)
	if l.snapshot() != LatchSDPTarget {
		t.Fatal("implausible packet latched")
	}
	l.observe(mustPkt(t, 1, 160, 7, 0, audio(80)), from, now)
	if l.snapshot() != LatchProvisional {
		t.Fatal("10 ms audio was refused")
	}
	l.observe(mustPkt(t, 2, 160, 8, 0, audio(160)), from, now)
	if l.snapshot() != LatchProvisional || l.droppedCount() == 0 {
		t.Fatal("second SSRC was accepted")
	}
	l.observe(mustPkt(t, 3, 320, 7, 101, []byte{1, 2, 3, 4}), from, now)
	l.observe(mustPkt(t, 4, 480, 7, 0, audio(160)), from, now)
	if l.snapshot() != LatchCommitted {
		t.Fatalf("telephone-event did not count, state %s", l.snapshot())
	}
}

func TestRTPLatchRetarget(t *testing.T) {
	l := newRTPLatch(LatchMatchAddress, nil, udp("10.0.0.1", 10000), 0, -1)
	now := time.Unix(1_700_000_000, 0)
	from := udp("10.0.0.1", 40000)
	for i := 0; i < 3; i++ {
		l.observe(mustPkt(t, uint16(i), uint32(i)*160, 7, 0, audio(160)), from, now)
	}
	l.retarget(udp("10.0.0.2", 18000))
	if l.snapshot() != LatchSDPTarget || l.dest().Port != 18000 {
		t.Fatalf("retarget %s %s", l.snapshot(), l.dest())
	}
	l.observe(mustPkt(t, 9, 160, 7, 0, audio(160)), from, now)
	if l.snapshot() != LatchProvisional || l.dest().Port != 40000 {
		t.Fatalf("relearn %s %s", l.snapshot(), l.dest())
	}
}

func TestRTCPLatch(t *testing.T) {
	sdp := udp("10.0.0.1", 10001)
	l := newRTCPLatch(LatchMatchAddress, nil, sdp)
	now := time.Unix(1_700_000_000, 0)
	a := udp("10.1.1.1", 5000)
	b := udp("10.2.2.2", 5001)
	l.observe(a, now)
	if l.snapshot() != LatchProvisional || l.dest().Port != 5000 {
		t.Fatalf("first %s %s", l.snapshot(), l.dest())
	}
	l.observe(b, now.Add(10*time.Millisecond))
	if l.dest().Port != 5000 {
		t.Fatal("RTCP latch switched sources")
	}
	l.advance(now.Add(100 * time.Millisecond))
	if l.snapshot() == LatchCommitted {
		t.Fatal("contradiction committed the RTCP latch")
	}
	l.observe(a, now.Add(20*time.Millisecond))
	if l.snapshot() != LatchCommitted || l.dest().Port != 5000 {
		t.Fatalf("two packets %s %s", l.snapshot(), l.dest())
	}
	l.observe(b, now.Add(time.Second))
	if l.dest().Port != 5000 {
		t.Fatal("RTCP latch moved after commit")
	}

	quiet := newRTCPLatch(LatchMatchPort, nil, sdp)
	quiet.observe(udp("10.9.9.9", 9), now)
	if quiet.snapshot() != LatchSDPTarget {
		t.Fatal("port match accepted a foreign RTCP source")
	}
	quiet.observe(udp("10.0.0.1", 9), now)
	quiet.advance(now.Add(100 * time.Millisecond))
	if quiet.snapshot() != LatchCommitted || quiet.dest().Port != 9 {
		t.Fatalf("quiet %s %s", quiet.snapshot(), quiet.dest())
	}

	off := newRTCPLatch(LatchMatchOff, nil, sdp)
	off.observe(a, now)
	off.advance(now.Add(time.Second))
	if off.snapshot() != LatchSDPTarget || off.dest().Port != 10001 {
		t.Fatalf("off %s %s", off.snapshot(), off.dest())
	}
}
