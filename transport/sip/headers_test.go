package sip

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func wantRaw(t *testing.T, msg sipRaw, name, value string) {
	t.Helper()
	if got := msg.hdr(name); got != value {
		t.Errorf("%s = %q, want %q\n%s", name, got, value, msg.raw)
	}
}

func wantCall(t *testing.T, c *Call, name, value string) {
	t.Helper()
	got, ok := c.Header(name)
	if !ok || got != value {
		t.Errorf("Header(%s) = %q ok=%v, want %q", name, got, ok, value)
	}
}

func wantEndpoint(t *testing.T, e *Endpoint, name, value string) {
	t.Helper()
	got, ok := e.Header(name)
	if !ok || got != value {
		t.Errorf("Endpoint.Header(%s) = %q ok=%v, want %q", name, got, ok, value)
	}
}

func refuseOwned(t *testing.T, msg sipRaw) {
	t.Helper()
	for _, name := range []string{"via", "contact", "cseq", "call-id", "from", "to", "max-forwards", "content-length", "route", "record-route"} {
		if strings.EqualFold(strings.TrimSpace(msg.hdr(name)), "spoof") {
			t.Errorf("%s was replaced\n%s", name, msg.raw)
		}
	}
}

func dialHeaders() []Header {
	names := []string{"Via", "Contact", "CSeq", "Call-ID", "From", "To", "Max-Forwards", "Content-Length", "Route", "Record-Route"}
	out := make([]Header, 0, len(names)+1)
	for _, name := range names {
		out = append(out, Header{Name: name, Value: "spoof"})
	}
	out = append(out, Header{Name: "X-Trace", Value: "dial"})
	return out
}

func (l *leg) sendInDialog(method, body string, cseq uint32, extra map[string]string) {
	l.t.Helper()
	if cseq > l.cseq {
		l.cseq = cseq
	}
	l.send(method, l.request, sip.GenerateBranch(), cseq, method, body, extra, true)
}

func TestDialHeadersOnInviteAckAndCancel(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		wantRaw(t, inv, "x-trace", "dial")
		refuseOwned(t, inv)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"X-Answer": "ok",
			"x-early":  "kept",
		})
		ack := u.waitMethod("ACK", 2*time.Second)
		wantRaw(t, ack, "x-trace", "dial")
		refuseOwned(t, ack)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri(), dialHeaders()...)
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "X-Answer", "ok")
	wantCall(t, call, "x-early", "kept")
	_ = call.Hangup(context.Background())
}

func TestDialCancelCopiesHeader(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		wantRaw(t, inv, "x-trace", "dial")
		u.respond(inv, 180, "Ringing", "", nil)
		time.Sleep(50 * time.Millisecond)
		cancel()
		cxl := u.waitMethod("CANCEL", 2*time.Second)
		wantRaw(t, cxl, "x-trace", "dial")
		refuseOwned(t, cxl)
		u.respond(cxl, 200, "OK", "", nil)
		u.respond(inv, 487, "Request Terminated", "", nil)
	}()
	_, err := ep.Dial(ctx, u.uri(), Header{Name: "x-trace", Value: "dial"}, Header{Name: "vIa", Value: "spoof"})
	if err == nil {
		t.Fatal("canceled dial returned a call")
	}
}

func TestDialEarlyAndFinalResponseHeaders(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	body := earlyBodyString(u.rtpPort(), true)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		// 100rel makes the client PRACK before we send 200, so the 183 is
		// recorded before the final response can overtake it.
		u.respond(inv, 183, "Session Progress", body, map[string]string{
			"Require": "100rel",
			"RSeq":    "1",
			"X-Early": "play",
		})
		pr := u.waitMethod("PRACK", 2*time.Second)
		u.respond(pr, 200, "OK", "", nil)
		u.respond(inv, 200, "OK", body, map[string]string{"X-Answer": "final"})
		_ = u.waitMethod("ACK", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-early", "play")
	wantCall(t, call, "X-ANSWER", "final")
	_ = call.Hangup(context.Background())
}

func TestPrackCarriesDialHeader(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 183, "Session Progress", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Require": "100rel",
			"RSeq":    "1",
			"X-Ring":  "provisional",
		})
		pr := u.waitMethod("PRACK", 2*time.Second)
		wantRaw(t, pr, "x-trace", "dial")
		u.respond(pr, 200, "OK", "", map[string]string{"X-Prack-OK": "yes"})
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri(), Header{Name: "X-Trace", Value: "dial"})
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-ring", "provisional")
	wantCall(t, call, "X-Prack-OK", "yes")
	_ = call.Hangup(context.Background())
}

func TestTransferHeadersOnRefer(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	ready := make(chan *Call, 1)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
		ref := u.waitMethod("REFER", 2*time.Second)
		wantRaw(t, ref, "x-refer", "blind")
		if strings.EqualFold(strings.TrimSpace(ref.hdr("via")), "spoof") {
			t.Errorf("REFER Via replaced\n%s", ref.raw)
		}
		u.respond(ref, 202, "Accepted", "", map[string]string{"Expires": "30", "X-Refer-OK": "202"})
		u.inDialog(ref, "NOTIFY", 2, "SIP/2.0 100 Trying\r\n", map[string]string{
			"Event":              "refer",
			"Subscription-State": "active",
			"Content-Type":       "message/sipfrag;version=2.0",
			"X-Note":             "trying",
		})
		call := <-ready
		deadline := time.Now().Add(2 * time.Second)
		for {
			if v, ok := call.Header("X-Note"); ok && v == "trying" {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("NOTIFY headers were not on the call before Transfer returned")
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		u.inDialog(ref, "NOTIFY", 4, "SIP/2.0 486 Busy Here\r\n", map[string]string{
			"Event":              "refer",
			"Subscription-State": "terminated;reason=noresource",
			"Content-Type":       "message/sipfrag;version=2.0",
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	ready <- call
	err = call.Transfer(ctx, "sip:carol@127.0.0.1:9", Header{Name: "X-Refer", Value: "blind"}, Header{Name: "Route", Value: "spoof"})
	if err == nil {
		t.Fatal("486 sipfrag succeeded")
	}
	wantCall(t, call, "x-note", "trying")
	wantCall(t, call, "x-refer-ok", "202")
	_ = call.Hangup(context.Background())
}

func TestAttendedTransferHeader(t *testing.T) {
	ep := startEP(t, baseConfig())
	other := startEP(t, baseConfig())
	got := acceptOne(other)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	consult, err := ep.Dial(ctx, endpointURI(t, other))
	if err != nil {
		t.Fatal(err)
	}
	<-got
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
		ref := u.waitMethod("REFER", 2*time.Second)
		wantRaw(t, ref, "x-attended", "yes")
		if !strings.Contains(strings.ToLower(ref.hdr("refer-to")), "replaces=") {
			t.Errorf("Refer-To %s", ref.hdr("refer-to"))
		}
		u.respond(ref, 202, "Accepted", "", map[string]string{"Expires": "1"})
	}()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	err = call.TransferAttended(ctx, consult, Header{Name: "X-Attended", Value: "yes"})
	if err == nil {
		t.Fatal("expired attended refer succeeded")
	}
	_ = call.Hangup(context.Background())
	_ = consult.Hangup(context.Background())
}

func TestInboundInviteHeadersBeforeAccept(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{"X-In": "invite"})
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if strings.EqualFold(strings.TrimSpace(ok.hdr("via")), "spoof") {
		t.Fatalf("response Via replaced\n%s", ok.raw)
	}
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-in", "invite")
	_ = call.Hangup(context.Background())
}

func TestReinviteAndUpdateExposeHeaders(t *testing.T) {
	ep := startEP(t, baseConfig())
	seen := make(chan string, 1)
	ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
		if !reinviteOf(req) || d.call == nil {
			return
		}
		v, _ := d.call.Header("X-Re")
		select {
		case seen <- v:
		default:
		}
	})
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), true, nil)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	call.SetHeader("X-Hold", "on")
	call.SetHeader("Via", "spoof")
	leg.sendInDialog("INVITE", "", leg.cseq+1, map[string]string{"X-Re": "again"})
	select {
	case got := <-seen:
		if got != "again" {
			t.Fatalf("re-INVITE header before the response: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-INVITE headers were not visible before the final response")
	}
	re := leg.waitCode(200, "INVITE", 2*time.Second)
	wantRaw(t, re, "x-hold", "on")
	refuseOwned(t, re)
	leg.ack(re)
	wantCall(t, call, "x-re", "again")

	leg.sendInDialog("UPDATE", "", leg.cseq+1, map[string]string{"X-Up": "mid"})
	upd := leg.waitCode(200, "UPDATE", 2*time.Second)
	wantRaw(t, upd, "x-hold", "on")
	wantCall(t, call, "X-Up", "mid")
	_ = call.Hangup(context.Background())
}

func TestByeAndOptionsHeaders(t *testing.T) {
	ep := startEP(t, baseConfig())
	ep.SetHeader("X-Opt", "endpoint")
	leg := newLeg(t, ep)
	leg.send("OPTIONS", leg.request, sip.GenerateBranch(), 1, "OPTIONS", "", map[string]string{"X-Ping": "out"}, false)
	opt := leg.waitCode(200, "OPTIONS", 2*time.Second)
	wantRaw(t, opt, "x-opt", "endpoint")
	wantEndpoint(t, ep, "x-ping", "out")
	leg.toTag = ""

	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{"X-In": "invite"})
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-in", "invite")

	call.SetHeader("x-bye", "local")
	leg.sendInDialog("OPTIONS", "", leg.cseq+1, map[string]string{"X-Ping": "dialog"})
	inOpt := leg.waitCode(200, "OPTIONS", 2*time.Second)
	wantRaw(t, inOpt, "x-bye", "local")
	wantCall(t, call, "X-Ping", "dialog")

	leg.sendInDialog("BYE", "", leg.cseq+1, map[string]string{"X-In-Bye": "peer"})
	bye := leg.waitCode(200, "BYE", 2*time.Second)
	wantRaw(t, bye, "x-bye", "local")
	wantCall(t, call, "x-in-bye", "peer")
}

func TestOutboundByeAndUpdateHeaders(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Allow": "INVITE, ACK, CANCEL, BYE, UPDATE",
		})
		_ = u.waitMethod("ACK", 2*time.Second)
		upd := u.waitMethod("UPDATE", 2*time.Second)
		wantRaw(t, upd, "x-hold", "yes")
		refuseOwned(t, upd)
		u.respond(upd, 200, "OK", pcmuDir(u.rtpPort(), "recvonly"), map[string]string{"X-Hold-OK": "up"})
		bye := u.waitMethod("BYE", 2*time.Second)
		wantRaw(t, bye, "x-bye", "down")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	call.SetHeader("X-Hold", "yes")
	call.SetHeader("Record-Route", "spoof")
	if err := call.Hold(ctx); err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-hold-ok", "up")
	call.SetHeader("X-Bye", "down")
	if err := call.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterHeaders(t *testing.T) {
	u := newUAS(t)
	cfg := baseConfig()
	cfg.SIPPort = freePort(t)
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	cfg.Register = &RegisterConfig{Registrar: registrarURI(u), Expires: time.Hour}
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ep.SetHeader("X-Reg", "yes")
	ep.SetHeader("Call-ID", "spoof")
	ep.SetHeader("Via", "spoof")
	go func() {
		first := u.waitMethod("REGISTER", 2*time.Second)
		wantRaw(t, first, "x-reg", "yes")
		refuseOwned(t, first)
		u.respond(first, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "n1"),
			"X-Challenge":      "once",
		})
		second := u.waitMethod("REGISTER", 2*time.Second)
		wantRaw(t, second, "x-reg", "yes")
		u.respond(second, 200, "OK", "", map[string]string{"X-Reg-OK": "set", "Expires": "3600"})
	}()
	if err := ep.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	wantEndpoint(t, ep, "x-challenge", "once")
	wantEndpoint(t, ep, "X-Reg-OK", "set")
}

func TestInboundPrackHeaders(t *testing.T) {
	ep := startEP(t, baseConfig())
	release := make(chan struct{})
	ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
		if reinviteOf(req) {
			return
		}
		if err := d.sendProvisional(180, "Ringing"); err != nil {
			t.Errorf("180: %v", err)
		}
		<-release
	})
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{"Supported": "100rel", "X-In": "invite"})
	ring := leg.waitCode(180, "INVITE", 2*time.Second)
	seq, _ := ring.cseq()
	rseq := strings.TrimSpace(ring.hdr("rseq"))
	leg.cseq = seq + 1
	leg.send("PRACK", leg.request, sip.GenerateBranch(), leg.cseq, "PRACK", "", map[string]string{
		"RAck":    rseq + " " + strconv.FormatUint(uint64(seq), 10) + " INVITE",
		"X-Prack": "in",
	}, true)
	leg.waitCode(200, "PRACK", 2*time.Second)
	close(release)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, call, "x-in", "invite")
	wantCall(t, call, "X-Prack", "in")
	_ = call.Hangup(context.Background())
}
