package sip

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestRTPRangeEdges(t *testing.T) {
	if err := checkRTP(10000, 10000); err != nil {
		t.Fatal(err)
	}
	if err := checkRTP(10000, 20000); err != nil {
		t.Fatal(err)
	}
	for _, span := range [][2]int{{10001, 20000}, {10000, 20001}, {20000, 10000}, {0, 10000}} {
		if err := checkRTP(span[0], span[1]); !errors.Is(err, ErrRTPRange) {
			t.Fatalf("range %d-%d err %v", span[0], span[1], err)
		}
	}
}

func TestAllocatorReservesEvenPair(t *testing.T) {
	min, max := findEvenSpan(t, 2)
	cfg := baseConfig()
	cfg.SIPPort = sipPortOutside(t, min, max)
	cfg.RTPPortMin = min
	cfg.RTPPortMax = max
	ep := startEP(t, cfg)

	first, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.LocalRTPPort != min || first.LocalRTCPPort != min+1 {
		t.Fatalf("first %d %d, want %d %d", first.LocalRTPPort, first.LocalRTCPPort, min, min+1)
	}
	if second.LocalRTPPort != min+2 || second.LocalRTCPPort != min+3 {
		t.Fatalf("second %d %d, want %d %d", second.LocalRTPPort, second.LocalRTCPPort, min+2, min+3)
	}
	if first.LocalRTPPort%2 != 0 || second.LocalRTPPort%2 != 0 {
		t.Fatal("odd rtp port")
	}
	for _, port := range []int{min, min + 1, min + 2, min + 3} {
		if _, ok := ep.used[port]; !ok {
			t.Fatalf("port %d is not reserved", port)
		}
	}
	if _, err := ep.AllocateRTP(context.Background()); !errors.Is(err, ErrPortsExhausted) {
		t.Fatalf("third alloc err %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ep.used[min]; ok {
		t.Fatal("closed rtp port stayed reserved")
	}
	if _, ok := ep.used[min+1]; ok {
		t.Fatal("closed rtcp port stayed reserved")
	}
	third, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.LocalRTPPort != min || third.LocalRTCPPort != min+1 {
		t.Fatalf("reused %d %d, want lowest %d %d", third.LocalRTPPort, third.LocalRTCPPort, min, min+1)
	}
	if third.LocalRTPPort == second.LocalRTPPort {
		t.Fatal("live calls share a port")
	}
}

func TestAllocatorSkipsPortThatWillNotBind(t *testing.T) {
	min, max := findEvenSpan(t, 2)
	blocker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: min})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	cfg := baseConfig()
	cfg.SIPPort = sipPortOutside(t, min, max)
	cfg.RTPPortMin = min
	cfg.RTPPortMax = max
	ep := startEP(t, cfg)

	got, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalRTPPort != min+2 {
		t.Fatalf("bound %d, want %d", got.LocalRTPPort, min+2)
	}
	if _, ok := ep.used[min]; ok {
		t.Fatal("unbindable port stayed reserved")
	}

	blocker.Close()
	next, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.LocalRTPPort != min {
		t.Fatalf("next %d, want freed lowest %d", next.LocalRTPPort, min)
	}
}

func TestAllocatorSkipsWhenRTCPWillNotBind(t *testing.T) {
	min, max := findEvenSpan(t, 2)
	blocker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: min + 1})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	cfg := baseConfig()
	cfg.SIPPort = sipPortOutside(t, min, max)
	cfg.RTPPortMin = min
	cfg.RTPPortMax = max
	ep := startEP(t, cfg)

	got, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalRTPPort != min+2 || got.LocalRTCPPort != min+3 {
		t.Fatalf("ports %d %d, want %d %d", got.LocalRTPPort, got.LocalRTCPPort, min+2, min+3)
	}
	if _, ok := ep.used[min]; ok {
		t.Fatal("rtp reservation leaked after rtcp bind failed")
	}
	if _, ok := ep.used[min+1]; ok {
		t.Fatal("rtcp reservation leaked after rtcp bind failed")
	}
}

func TestAllocatorSingleEvenPort(t *testing.T) {
	min, max := findEvenSpan(t, 1)
	if min != max {
		t.Fatalf("span %d-%d", min, max)
	}
	cfg := baseConfig()
	cfg.SIPPort = sipPortOutside(t, min, max)
	cfg.RTPPortMin = min
	cfg.RTPPortMax = max
	ep := startEP(t, cfg)

	got, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalRTPPort != min || got.LocalRTCPPort != min+1 {
		t.Fatalf("ports %d %d", got.LocalRTPPort, got.LocalRTCPPort)
	}
	if _, err := ep.AllocateRTP(context.Background()); !errors.Is(err, ErrPortsExhausted) {
		t.Fatalf("second err %v", err)
	}

	held, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: min})
	if err == nil {
		held.Close()
		t.Fatal("allocated port was not bound")
	}
}

func TestAllocatorReleasesWhenBindFails(t *testing.T) {
	min, max := findEvenSpan(t, 1)
	blocker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: min})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	cfg := baseConfig()
	cfg.SIPPort = sipPortOutside(t, min, max)
	cfg.RTPPortMin = min
	cfg.RTPPortMax = max
	ep := startEP(t, cfg)
	if _, err := ep.AllocateRTP(context.Background()); err == nil {
		t.Fatal("allocated a port that was already bound")
	}
	if len(ep.used) != 0 {
		t.Fatalf("reservation leaked: %v", ep.used)
	}
}

func findEvenSpan(t *testing.T, pairs int) (int, int) {
	t.Helper()
	need := pairs * 2
	for base := 25000; base <= 60000; base += 2 {
		end := base + need - 1
		if end > 65535 {
			break
		}
		held := make([]*net.UDPConn, 0, need)
		ok := true
		for port := base; port <= end; port++ {
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
			if err != nil {
				ok = false
				break
			}
			held = append(held, conn)
		}
		for _, conn := range held {
			conn.Close()
		}
		if ok && len(held) == need {
			return base, base + (pairs-1)*2
		}
	}
	t.Fatal("no free even port span")
	return 0, 0
}

func sipPortOutside(t *testing.T, min, max int) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		port := freePort(t)
		if port < min || port > max+1 {
			return port
		}
	}
	t.Fatal("no sip port outside the rtp span")
	return 0
}
