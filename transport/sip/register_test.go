package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func registrarURI(u *uas) string {
	return "sip:" + u.conn.LocalAddr().String()
}

func TestRegisterDigestOnce(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	go func() {
		first := u.waitMethod("REGISTER", 2*time.Second)
		if !strings.HasPrefix(first.first, "REGISTER "+registrarURI(u)) {
			t.Errorf("request uri %s", first.first)
		}
		if first.hdr("authorization") != "" {
			t.Errorf("first REGISTER carried Authorization")
		}
		if !strings.Contains(first.hdr("to"), "sip:bot@sip.example.com") {
			t.Errorf("To %s", first.hdr("to"))
		}
		if !strings.Contains(first.hdr("contact"), "sip:bot@") || !strings.Contains(first.hdr("contact"), "transport=udp") {
			t.Errorf("Contact %s", first.hdr("contact"))
		}
		if first.hdr("expires") != "3600" {
			t.Errorf("Expires %q", first.hdr("expires"))
		}
		firstSeq, method := first.cseq()
		if method != "REGISTER" || firstSeq == 0 {
			t.Errorf("cseq %d %s", firstSeq, method)
		}
		u.respond(first, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "reg1"),
		})
		second := u.waitMethod("REGISTER", 2*time.Second)
		if second.hdr("call-id") != first.hdr("call-id") {
			t.Errorf("call-id %s then %s", first.hdr("call-id"), second.hdr("call-id"))
		}
		secondSeq, _ := second.cseq()
		if secondSeq != firstSeq+1 {
			t.Errorf("cseq %d then %d", firstSeq, secondSeq)
		}
		if second.hdr("authorization") == "" {
			t.Errorf("retry missing Authorization")
		}
		u.respond(second, 200, "OK", "", map[string]string{"Expires": "3600"})
		if msg, ok := u.peekMethod("REGISTER", 3*time.Second); ok {
			u.respond(msg, 200, "OK", "", map[string]string{"Expires": "3600"})
		}
	}()
	ep := startEP(t, cfg)
	if calls := ep.dialogs.calls(); len(calls) != 0 {
		t.Fatalf("REGISTER created %d calls", len(calls))
	}
	ep.mu.Lock()
	used := len(ep.used)
	ep.mu.Unlock()
	if used != 0 {
		t.Fatalf("REGISTER allocated %d rtp ports", used)
	}
}

func TestRegisterSecondChallengeFails(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.SIPPort = freePort(t)
	cfg.Auth = &DigestAuth{Password: "s3cret"}
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := u.waitMethod("REGISTER", 2*time.Second)
		u.respond(first, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "a"),
		})
		second := u.waitMethod("REGISTER", 2*time.Second)
		u.respond(second, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "b"),
		})
		if msg, ok := u.peekMethod("REGISTER", 300*time.Millisecond); ok {
			t.Errorf("third REGISTER\n%s", msg.raw)
		}
	}()
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = ep.Start(context.Background())
	if err == nil {
		t.Fatal("second challenge succeeded")
	}
	if _, addrErr := ep.Addresses(); addrErr == nil {
		t.Fatal("failed Start left the endpoint up")
	}
	<-done
	_ = ep.Close()
}

func TestRegisterBadRealmFails(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.SIPPort = freePort(t)
	cfg.Auth = &DigestAuth{Password: "s3cret"}
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := u.waitMethod("REGISTER", 2*time.Second)
		u.respond(first, 407, "Proxy Authentication Required", "", map[string]string{
			"Proxy-Authenticate": challenge("other.example", "p"),
		})
		if msg, ok := u.peekMethod("REGISTER", 300*time.Millisecond); ok {
			t.Errorf("answered a foreign realm\n%s", msg.raw)
		}
	}()
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = ep.Start(context.Background()); err == nil {
		t.Fatal("foreign realm succeeded")
	}
	<-done
	_ = ep.Close()
}

func TestRegisterRefreshHalf(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u)}
	elapsed := make(chan time.Duration, 1)
	go func() {
		first := u.waitMethod("REGISTER", 2*time.Second)
		if first.hdr("expires") != "" {
			t.Errorf("Expires sent %q", first.hdr("expires"))
		}
		id := first.hdr("call-id")
		seq, _ := first.cseq()
		sent := time.Now()
		u.respond(first, 200, "OK", "", map[string]string{"Expires": "2"})
		second := u.waitMethod("REGISTER", 4*time.Second)
		if second.hdr("call-id") != id {
			t.Errorf("call-id changed %s", second.hdr("call-id"))
		}
		next, method := second.cseq()
		if method != "REGISTER" || next != seq+1 {
			t.Errorf("refresh cseq %d %s, first %d", next, method, seq)
		}
		u.respond(second, 200, "OK", "", map[string]string{"Expires": "2"})
		elapsed <- time.Since(sent)
		if msg, ok := u.peekMethod("REGISTER", 3*time.Second); ok {
			u.respond(msg, 200, "OK", "", map[string]string{"Expires": "2"})
		}
	}()
	_ = startEP(t, cfg)
	select {
	case got := <-elapsed:
		if got < 600*time.Millisecond || got > 3*time.Second {
			t.Fatalf("refresh after %s, want half of 2s", got)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("refresh did not arrive")
	}
}

func TestRegisterExpiresZeroOnClose(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	ready := make(chan struct{})
	got := make(chan sipRaw, 1)
	go func() {
		first := u.waitMethod("REGISTER", 2*time.Second)
		if first.hdr("expires") != "3600" {
			t.Errorf("Expires %q", first.hdr("expires"))
		}
		u.respond(first, 200, "OK", "", map[string]string{"Expires": "3600"})
		close(ready)
		second := u.waitMethod("REGISTER", 3*time.Second)
		got <- second
		u.respond(second, 200, "OK", "", nil)
	}()
	ep := startEP(t, cfg)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("registrar did not answer")
	}
	if err := ep.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if msg.hdr("expires") != "0" {
			t.Fatalf("Expires %q", msg.hdr("expires"))
		}
		if !strings.HasPrefix(msg.first, "REGISTER ") {
			t.Fatalf("request %s", msg.first)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Expires 0 was not sent")
	}
}

func TestRegisterDigestTimeoutAdvancesCSeq(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.SIPPort = freePort(t)
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	firstC := make(chan sipRaw, 1)
	digestC := make(chan sipRaw, 1)
	go func() {
		first := u.waitMethod("REGISTER", 2*time.Second)
		firstC <- first
		u.respond(first, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "reg-to"),
		})
		digest := u.waitMethod("REGISTER", 2*time.Second)
		digestC <- digest
	}()
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err = ep.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start err %v", err)
	}
	first := <-firstC
	digest := <-digestC
	firstSeq, _ := first.cseq()
	digestSeq, _ := digest.cseq()
	if digest.hdr("call-id") != first.hdr("call-id") {
		t.Fatalf("call-id %s then %s", first.hdr("call-id"), digest.hdr("call-id"))
	}
	if digestSeq != firstSeq+1 {
		t.Fatalf("digest cseq %d, first %d", digestSeq, firstSeq)
	}
	if digest.hdr("authorization") == "" {
		t.Fatal("digest retry missing Authorization")
	}
	nextC := make(chan sipRaw, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			msg := u.waitMethod("REGISTER", time.Until(deadline))
			seq, _ := msg.cseq()
			if seq == digestSeq {
				continue
			}
			nextC <- msg
			u.respond(msg, 200, "OK", "", map[string]string{"Expires": "3600"})
			if again, ok := u.peekMethod("REGISTER", 3*time.Second); ok {
				u.respond(again, 200, "OK", "", nil)
			}
			return
		}
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := ep.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	next := <-nextC
	nextSeq, method := next.cseq()
	if method != "REGISTER" {
		t.Fatalf("method %s", method)
	}
	if next.hdr("call-id") != digest.hdr("call-id") {
		t.Fatalf("call-id reused as %s", next.hdr("call-id"))
	}
	if nextSeq <= digestSeq {
		t.Fatalf("cseq %d reuses digest cseq %d", nextSeq, digestSeq)
	}
}

func TestRegisterTimeoutFailsStart(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.SIPPort = freePort(t)
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	go func() {
		_ = u.waitMethod("REGISTER", 2*time.Second)
	}()
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = ep.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start err %v", err)
	}
	if _, addrErr := ep.Addresses(); addrErr == nil {
		t.Fatal("timed out Start left the endpoint up")
	}
	_ = ep.Close()
}

func TestOptionsOK(t *testing.T) {
	ep := startEP(t, baseConfig())
	msg, ok := exchangeOptions(t, ep, time.Second)
	if !ok {
		t.Fatal("OPTIONS got no response")
	}
	if msg.code != 200 {
		t.Fatalf("status %d %s", msg.code, msg.first)
	}
	if msg.hdr("allow") != allowMethods {
		t.Fatalf("Allow %q", msg.hdr("allow"))
	}
	if msg.hdr("supported") != "100rel" {
		t.Fatalf("Supported %q", msg.hdr("supported"))
	}
	if msg.hdr("accept") != "application/sdp" {
		t.Fatalf("Accept %q", msg.hdr("accept"))
	}
	if strings.TrimSpace(msg.body) != "" {
		t.Fatalf("body %q", msg.body)
	}
	if calls := ep.dialogs.calls(); len(calls) != 0 {
		t.Fatalf("OPTIONS created %d calls", len(calls))
	}
}

func TestOptionsDropped(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowFrom = []string{"10.9.8.7"}
	ep := startEP(t, cfg)
	if msg, ok := exchangeOptions(t, ep, 400*time.Millisecond); ok {
		t.Fatalf("disallowed OPTIONS got %s", msg.first)
	}
}

func exchangeOptions(t *testing.T, ep *Endpoint, wait time.Duration) (sipRaw, bool) {
	t.Helper()
	addrs, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	src := conn.LocalAddr().(*net.UDPAddr)
	dst := &net.UDPAddr{IP: addrs.AdvertiseIP, Port: addrs.AdvertiseSIPPort}
	probe := fmt.Sprintf("OPTIONS sip:bot@%s SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bKopt;rport\r\nFrom: <sip:alice@sip.example.com>;tag=opt\r\nTo: <sip:bot@sip.example.com>\r\nCall-ID: opt@127.0.0.1\r\nCSeq: 1 OPTIONS\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n", dst.String(), src.Port)
	if _, err := conn.WriteToUDP([]byte(probe), dst); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return sipRaw{}, false
	}
	msg, err := parseSIP(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	return msg, true
}
