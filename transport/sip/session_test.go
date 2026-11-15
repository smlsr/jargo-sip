package sip

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionTimerOff(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		if inv.hdr("session-expires") != "" {
			t.Errorf("Session-Expires %q", inv.hdr("session-expires"))
		}
		if strings.Contains(strings.ToLower(inv.hdr("supported")), "timer") {
			t.Errorf("Supported %q", inv.hdr("supported"))
		}
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestSessionTimerRefreshHangup(t *testing.T) {
	cfg := baseConfig()
	cfg.SessionTimer = 2 * time.Second
	ep := startEP(t, cfg)
	u := newUAS(t)
	elapsed := make(chan time.Duration, 1)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		if !strings.Contains(inv.hdr("session-expires"), "2") || !strings.Contains(inv.hdr("session-expires"), "refresher=uac") {
			t.Errorf("Session-Expires %q", inv.hdr("session-expires"))
		}
		if !strings.Contains(inv.hdr("supported"), "100rel") || !strings.Contains(inv.hdr("supported"), "timer") {
			t.Errorf("Supported %q", inv.hdr("supported"))
		}
		sent := time.Now()
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Allow":           "INVITE, ACK, CANCEL, BYE, UPDATE, OPTIONS",
			"Session-Expires": "2;refresher=uac",
		})
		_ = u.waitMethod("ACK", 2*time.Second)
		upd := u.waitMethod("UPDATE", 4*time.Second)
		elapsed <- time.Since(sent)
		if upd.body != "" {
			t.Errorf("refresh body %q", upd.body)
		}
		if upd.hdr("content-type") != "" {
			t.Errorf("refresh content-type %q", upd.hdr("content-type"))
		}
		u.respond(upd, 500, "Server Internal Error", "", nil)
		_ = u.waitMethod("BYE", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-elapsed:
		if got < 600*time.Millisecond || got > 3500*time.Millisecond {
			t.Fatalf("refresh after %s, want half of 2s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session refresh did not arrive")
	}
	select {
	case <-call.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("failed refresh left the call up")
	}
}

func TestSessionTimerReinvite(t *testing.T) {
	cfg := baseConfig()
	cfg.SessionTimer = 2 * time.Second
	ep := startEP(t, cfg)
	u := newUAS(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inv := u.waitMethod("INVITE", 2*time.Second)
		offered, err := parseSDPText(inv.body)
		if err != nil {
			t.Errorf("offer: %v", err)
		}
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Allow":           "INVITE, ACK, BYE, OPTIONS",
			"Session-Expires": "2;refresher=uac",
		})
		_ = u.waitMethod("ACK", 2*time.Second)
		re := u.waitMethod("INVITE", 4*time.Second)
		if _, method := re.cseq(); method != "INVITE" {
			t.Errorf("refresh method %s", method)
		}
		next, err := parseSDPText(re.body)
		if err != nil {
			t.Errorf("refresh sdp: %v", err)
		} else if next.Version <= offered.Version || next.RTPPort != offered.RTPPort || next.Direction != offered.Direction || !next.Connection.Equal(offered.Connection) {
			t.Errorf("offer %+v refresh %+v", offered, next)
		}
		u.respond(re, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-INVITE refresh did not finish")
	}
	select {
	case <-call.Done():
		t.Fatal("refresh hangup")
	default:
	}
	_ = call.Hangup(context.Background())
}

func TestSessionTimer491Retries(t *testing.T) {
	cfg := baseConfig()
	cfg.SessionTimer = 20 * time.Second
	ep := startEP(t, cfg)
	u := newUAS(t)
	retried := make(chan struct{})
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Allow":           "INVITE, ACK, CANCEL, BYE, UPDATE, OPTIONS",
			"Session-Expires": "20;refresher=uac",
		})
		_ = u.waitMethod("ACK", 2*time.Second)
		upd := u.waitMethod("UPDATE", 12*time.Second)
		updSeq, _ := upd.cseq()
		u.respond(upd, 491, "Request Pending", "", nil)
		again := u.waitMethod("UPDATE", 6*time.Second)
		againSeq, method := again.cseq()
		if method != "UPDATE" || againSeq <= updSeq {
			t.Errorf("retry cseq %d %s after %d", againSeq, method, updSeq)
		}
		if again.body != "" {
			t.Errorf("retry body %q", again.body)
		}
		u.respond(again, 200, "OK", "", nil)
		close(retried)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-retried:
	case <-time.After(18 * time.Second):
		t.Fatal("491 was not retried")
	}
	select {
	case <-call.Done():
		t.Fatal("491 hung up the call")
	case <-time.After(300 * time.Millisecond):
	}
	_ = call.Hangup(context.Background())
}

func TestInboundSessionEcho(t *testing.T) {
	cfg := baseConfig()
	cfg.SessionTimer = 30 * time.Second
	ep := startEP(t, cfg)
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), true, map[string]string{
		"Session-Expires": "30;refresher=uas",
		"Supported":       "timer",
	})
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if !strings.Contains(ok.hdr("session-expires"), "30") || !strings.Contains(ok.hdr("session-expires"), "refresher=uas") {
		t.Fatalf("Session-Expires %q", ok.hdr("session-expires"))
	}
	if !strings.Contains(ok.hdr("supported"), "timer") || !strings.Contains(ok.hdr("supported"), "100rel") {
		t.Fatalf("Supported %q", ok.hdr("supported"))
	}
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}
