package sip

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/gojargo/jargo/audio/g711"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
)

func TestDialOfferAckAndCSeq(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		assertOffer(t, inv)
		if strings.Contains(strings.ToLower(inv.hdr("require")), "100rel") {
			t.Errorf("Require: %q", inv.hdr("require"))
		}
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		ack := u.waitMethod("ACK", 2*time.Second)
		invSeq, _ := inv.cseq()
		ackSeq, ackMethod := ack.cseq()
		if ackMethod != "ACK" || ackSeq != invSeq {
			t.Errorf("ack cseq %d %s, invite %d", ackSeq, ackMethod, invSeq)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	if call.Remote().CallID == "" || call.Remote().RemoteTarget == "" {
		t.Fatalf("party %+v", call.Remote())
	}
	_ = call.Hangup(context.Background())
}

func TestDialDigestOnce(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	ep := startEP(t, cfg)
	u := newUAS(t)
	go func() {
		first := u.waitMethod("INVITE", 2*time.Second)
		u.respond(first, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "n1"),
		})
		second := u.waitMethod("INVITE", 2*time.Second)
		if second.hdr("authorization") == "" {
			t.Errorf("retry missing Authorization")
		}
		if _, method := second.cseq(); method != "INVITE" {
			t.Errorf("retry method %s", method)
		}
		u.respond(second, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "n2"),
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := ep.Dial(ctx, u.uri())
	if err == nil {
		t.Fatal("second challenge succeeded")
	}
	if msg, ok := u.peekMethod("INVITE", 300*time.Millisecond); ok {
		t.Fatalf("third invite\n%s", msg.raw)
	}
}

func TestDialNilAuthFails(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 401, "Unauthorized", "", map[string]string{
			"WWW-Authenticate": challenge("sip.example.com", "n1"),
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ep.Dial(ctx, u.uri())
	if err == nil {
		t.Fatal("nil auth dial succeeded")
	}
	if msg, ok := u.peekMethod("INVITE", 250*time.Millisecond); ok {
		t.Fatalf("retried without credentials\n%s", msg.raw)
	}
}

func TestDialProxyAuthOnce(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	ep := startEP(t, cfg)
	u := newUAS(t)
	go func() {
		first := u.waitMethod("INVITE", 2*time.Second)
		firstSeq, _ := first.cseq()
		u.respond(first, 407, "Proxy Authentication Required", "", map[string]string{
			"Proxy-Authenticate": challenge("sip.example.com", "p1"),
		})
		second := u.waitMethod("INVITE", 2*time.Second)
		secondSeq, _ := second.cseq()
		if second.hdr("proxy-authorization") == "" {
			t.Errorf("missing Proxy-Authorization")
		}
		if secondSeq == firstSeq {
			t.Errorf("retry kept cseq %d", secondSeq)
		}
		u.respond(second, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		ack := u.waitMethod("ACK", 2*time.Second)
		ackSeq, _ := ack.cseq()
		if ackSeq != secondSeq {
			t.Errorf("ack %d invite %d", ackSeq, secondSeq)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestDialEarlyMedia(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 20 * time.Millisecond
	ep := startEP(t, cfg)
	u := newUAS(t)
	payload := bytes.Repeat([]byte{0x21}, frameSamples)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		desc, err := parseSDPText(inv.body)
		if err != nil {
			t.Errorf("offer: %v", err)
			return
		}
		body := earlyBodyString(u.rtpPort(), true)
		u.respond(inv, 183, "Session Progress", body, nil)
		for i := 0; i < 4; i++ {
			u.sendRTP(desc, payload, uint16(i+1))
		}
		time.Sleep(40 * time.Millisecond)
		u.respond(inv, 200, "OK", body, nil)
		_ = u.waitMethod("ACK", 2*time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	frame := readCallPCM(t, call, time.Second)
	want := g711.DecodeULaw(payload)
	if len(frame) != frameBytes || !bytes.Equal(frame, want) {
		t.Fatalf("early frame len %d", len(frame))
	}
	_ = call.Hangup(context.Background())
}

func TestDialCancel(t *testing.T) {
	cfg := baseConfig()
	cfg.RTPPortMin = 42000
	cfg.RTPPortMax = 42000
	ep := startEP(t, cfg)
	u := newUAS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ringing := make(chan struct{})
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		invSeq, _ := inv.cseq()
		u.respond(inv, 180, "Ringing", "", nil)
		close(ringing)
		got := u.waitMethod("CANCEL", 2*time.Second)
		seq, method := got.cseq()
		if method != "CANCEL" || seq != invSeq {
			t.Errorf("cancel cseq %d %s, invite %d", seq, method, invSeq)
		}
		u.respond(got, 200, "OK", "", nil)
		u.respond(inv, 487, "Request Terminated", "", nil)
	}()
	go func() {
		<-ringing
		cancel()
	}()
	_, err := ep.Dial(ctx, u.uri())
	if err == nil {
		t.Fatal("cancelled dial returned a call")
	}
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = pair.Close()
}

func TestDialFailureReleasesPort(t *testing.T) {
	cfg := baseConfig()
	cfg.RTPPortMin = 42010
	cfg.RTPPortMax = 42010
	ep := startEP(t, cfg)
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 486, "Busy Here", "", nil)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ep.Dial(ctx, u.uri())
	if err == nil {
		t.Fatal("486 returned a call")
	}
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = pair.Close()
}

func TestDialReliable183(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		invSeq, _ := inv.cseq()
		u.respond(inv, 183, "Session Progress", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Require": "100rel",
			"RSeq":    "1",
		})
		pr := u.waitMethod("PRACK", 2*time.Second)
		rack := pr.hdr("rack")
		if rack != fmt.Sprintf("1 %d INVITE", invSeq) {
			t.Errorf("RAck %q", rack)
		}
		u.respond(pr, 200, "OK", "", nil)
		u.respond(inv, 183, "Session Progress", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Require": "100rel",
			"RSeq":    "1",
		})
		if extra, ok := u.peekMethod("PRACK", 250*time.Millisecond); ok {
			t.Errorf("second PRACK\n%s", extra.raw)
		}
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		ack := u.waitMethod("ACK", 2*time.Second)
		ackSeq, _ := ack.cseq()
		if ackSeq != invSeq {
			t.Errorf("ack cseq %d invite %d", ackSeq, invSeq)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestDialAuthEndpoint(t *testing.T) {
	calleeCfg := baseConfig()
	calleeCfg.RequireAuth = true
	calleeCfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	callee := startEP(t, calleeCfg)
	callerCfg := baseConfig()
	callerCfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	caller := startEP(t, callerCfg)
	got := make(chan *Call, 1)
	go func() {
		c, err := callee.Accept()
		if err == nil {
			got <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := caller.Dial(ctx, endpointURI(t, callee))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("callee did not accept")
	}
	_ = call.Hangup(context.Background())
}

func TestBlindRefer(t *testing.T) {
	a := startEP(t, baseConfig())
	b := startEP(t, baseConfig())
	c := startEP(t, baseConfig())
	bGot := acceptOne(b)
	cGot := acceptOne(c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ab, err := a.Dial(ctx, endpointURI(t, b))
	if err != nil {
		t.Fatal(err)
	}
	bCall := <-bGot
	replaced := make(chan *Call, 1)
	a.OnReplaced(func(old, new *Call) {
		if old == nil || new == nil {
			t.Errorf("replaced old %v new %v", old, new)
			replaced <- new
			return
		}
		select {
		case <-old.Done():
			t.Error("old call ended before OnReplaced")
		default:
		}
		replaced <- new
	})
	if err := bCall.Transfer(ctx, endpointURI(t, c)); err != nil {
		t.Fatal(err)
	}
	var fresh *Call
	select {
	case fresh = <-replaced:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReplaced did not run")
	}
	select {
	case <-cGot:
	case <-time.After(2 * time.Second):
		t.Fatal("new call was not accepted")
	}
	select {
	case <-fresh.Done():
		t.Fatal("new call is not live")
	default:
	}
	select {
	case <-ab.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("original call stayed up")
	}
	_ = fresh.Hangup(context.Background())
}

func TestAttendedRefer(t *testing.T) {
	a := startEP(t, baseConfig())
	b := startEP(t, baseConfig())
	c := startEP(t, baseConfig())
	bGot := acceptOne(b)
	cGot := acceptOne(c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ab, err := a.Dial(ctx, endpointURI(t, b))
	if err != nil {
		t.Fatal(err)
	}
	ac, err := a.Dial(ctx, endpointURI(t, c))
	if err != nil {
		t.Fatal(err)
	}
	<-bGot
	<-cGot
	bNext := acceptOne(b)
	replaced := make(chan *Call, 1)
	b.OnReplaced(func(old, new *Call) {
		if old == nil || new == nil {
			t.Errorf("replaced old %v new %v", old, new)
		} else {
			select {
			case <-old.Done():
				t.Error("old call ended before OnReplaced")
			default:
			}
		}
		replaced <- new
	})
	if err := ac.TransferAttended(ctx, ab); err != nil {
		t.Fatal(err)
	}
	var fresh *Call
	select {
	case fresh = <-replaced:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReplaced did not run")
	}
	select {
	case <-bNext:
	case <-time.After(2 * time.Second):
		t.Fatal("replaced call was not accepted")
	}
	select {
	case <-fresh.Done():
		t.Fatal("replaced call is not live")
	default:
	}
	select {
	case <-ab.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("consult call stayed up")
	}
	_ = fresh.Hangup(context.Background())
}

func TestTransferRejectsMissingCall(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := call.TransferAttended(ctx, nil); err == nil {
		t.Fatal("nil target was accepted")
	}
	if msg, ok := leg.peekMethod("REFER", 200*time.Millisecond); ok {
		t.Fatalf("REFER left\n%s", msg.raw)
	}
	_ = call.Hangup(context.Background())
	if err := call.Transfer(context.Background(), "sip:carol@127.0.0.1:9"); err == nil {
		t.Fatal("ended call transferred")
	}
}

func TestReferBadTarget(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	leg.cseq++
	leg.send("REFER", leg.request, sip.GenerateBranch(), leg.cseq, "REFER", "", map[string]string{
		"Refer-To": "<sip:carol@127.0.0.1;method=BYE>",
	}, true)
	bad := leg.waitCode(400, "REFER", 2*time.Second)
	if bad.code != 400 {
		t.Fatalf("code %d", bad.code)
	}
	select {
	case <-call.Done():
		t.Fatal("bad refer ended the call")
	default:
	}
	_ = call.Hangup(context.Background())
}

func TestReplacesUnknown(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{
		"Replaces": "missing-call;to-tag=aa;from-tag=bb",
	})
	leg.waitCode(481, "INVITE", 2*time.Second)
}

func TestReplacesEarly(t *testing.T) {
	t.Run("no early-only", func(t *testing.T) {
		ep := startEP(t, baseConfig())
		var once atomic.Bool
		ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
			if reinviteOf(req) || !once.CompareAndSwap(false, true) {
				return
			}
			other := newLeg(t, ep)
			other.invite(pcmuOffer(other.rtpPort()), false, map[string]string{
				"Replaces": replacesValue(d, false),
			})
			other.waitCode(491, "INVITE", 2*time.Second)
		})
		leg := newLeg(t, ep)
		call := establish(t, ep, leg, false)
		_ = call.Hangup(context.Background())
	})
	t.Run("early-only", func(t *testing.T) {
		ep := startEP(t, baseConfig())
		replaced := make(chan *Call, 1)
		ep.OnReplaced(func(old, new *Call) {
			if old != nil {
				select {
				case <-old.Done():
					t.Error("old call already ended")
				default:
				}
			}
			replaced <- new
		})
		var once atomic.Bool
		var first *leg
		ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
			if reinviteOf(req) || !once.CompareAndSwap(false, true) {
				return
			}
			other := newLeg(t, ep)
			other.invite(pcmuOffer(other.rtpPort()), false, map[string]string{
				"Replaces": replacesValue(d, true),
			})
			ok := other.waitCode(200, "INVITE", 2*time.Second)
			other.ack(ok)
			if first != nil {
				first.waitCode(487, "INVITE", 2*time.Second)
			}
		})
		accepted := make(chan *Call, 1)
		go func() {
			c, err := ep.Accept()
			if err == nil {
				accepted <- c
			}
		}()
		first = newLeg(t, ep)
		first.invite(pcmuOffer(first.rtpPort()), false, nil)
		var call *Call
		select {
		case call = <-replaced:
		case <-time.After(3 * time.Second):
			t.Fatal("OnReplaced did not run")
		}
		select {
		case got := <-accepted:
			if got != call {
				t.Fatal("Accept returned a different call")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Accept did not return the new call")
		}
		_ = call.Hangup(context.Background())
	})
}

func TestReferFailureLeavesCall(t *testing.T) {
	cfg := baseConfig()
	ep := startEP(t, cfg)
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), map[string]string{
			"Allow": "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, REFER, NOTIFY, OPTIONS",
		})
		_ = u.waitMethod("ACK", 2*time.Second)
		ref := u.waitMethod("REFER", 2*time.Second)
		if strings.Contains(strings.ToLower(ref.hdr("refer-to")), "replaces=") {
			t.Errorf("blind refer carried Replaces: %s", ref.hdr("refer-to"))
		}
		u.respond(ref, 202, "Accepted", "", map[string]string{"Expires": "30"})
		u.notify(ref, "SIP/2.0 486 Busy Here\r\n", "terminated;reason=noresource")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	err = call.Transfer(ctx, "sip:carol@127.0.0.1:9")
	if err == nil {
		t.Fatal("486 sipfrag succeeded")
	}
	select {
	case <-call.Done():
		t.Fatal("failed refer ended the call")
	default:
	}
	_ = call.Hangup(context.Background())
}

func TestReferExpires(t *testing.T) {
	ep := startEP(t, baseConfig())
	u := newUAS(t)
	go func() {
		inv := u.waitMethod("INVITE", 2*time.Second)
		u.respond(inv, 200, "OK", earlyBodyString(u.rtpPort(), true), nil)
		_ = u.waitMethod("ACK", 2*time.Second)
		ref := u.waitMethod("REFER", 2*time.Second)
		u.respond(ref, 202, "Accepted", "", map[string]string{"Expires": "1"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	err = call.Transfer(ctx, "sip:carol@127.0.0.1:9")
	if err == nil {
		t.Fatal("expired refer succeeded")
	}
	select {
	case <-call.Done():
		t.Fatal("expired refer ended the call")
	default:
	}
	_ = call.Hangup(context.Background())
}

func TestAttendedReferCarriesReplaces(t *testing.T) {
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
		referTo := ref.hdr("refer-to")
		if !strings.Contains(strings.ToLower(referTo), "replaces=") {
			t.Errorf("Refer-To %s", referTo)
		}
		party := consult.Remote()
		if !strings.Contains(referTo, party.CallID) || !strings.Contains(referTo, party.RemoteTag) || !strings.Contains(referTo, party.LocalTag) {
			t.Errorf("Replaces tags missing in %s party %+v", referTo, party)
		}
		if !strings.Contains(strings.ToLower(ref.hdr("referred-by")), "sip:bot@sip.example.com") {
			t.Errorf("Referred-By %s", ref.hdr("referred-by"))
		}
		u.respond(ref, 202, "Accepted", "", map[string]string{"Expires": "1"})
	}()
	call, err := ep.Dial(ctx, u.uri())
	if err != nil {
		t.Fatal(err)
	}
	err = call.TransferAttended(ctx, consult)
	if err == nil {
		t.Fatal("expired attended refer succeeded")
	}
	select {
	case <-call.Done():
		t.Fatal("failed attended refer ended the call")
	default:
	}
	select {
	case <-consult.Done():
		t.Fatal("consult call ended")
	default:
	}
	_ = call.Hangup(context.Background())
	_ = consult.Hangup(context.Background())
}

func assertOffer(t *testing.T, inv sipRaw) {
	t.Helper()
	if !strings.Contains(inv.hdr("supported"), "100rel") {
		t.Fatalf("Supported %q", inv.hdr("supported"))
	}
	desc, err := parseSDPText(inv.body)
	if err != nil {
		t.Fatal(err)
	}
	if desc.PTime != 20 || desc.Direction != DirectionSendRecv {
		t.Fatalf("ptime %d dir %s", desc.PTime, desc.Direction)
	}
	got := map[string]bool{}
	for _, f := range desc.Formats {
		got[f.Name] = true
		if f.Name == "telephone-event" && (f.Clock != 8000 || f.Payload != 101) {
			t.Fatalf("telephone-event %+v", f)
		}
	}
	for _, name := range []string{"PCMU", "PCMA", "telephone-event"} {
		if !got[name] {
			t.Fatalf("offer missing %s %+v", name, desc.Formats)
		}
	}
}

func replacesValue(d *dialog, early bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := fmt.Sprintf("%s;to-tag=%s;from-tag=%s", d.party.CallID, d.party.LocalTag, d.party.RemoteTag)
	if early {
		v += ";early-only"
	}
	return v
}

func endpointURI(t *testing.T, ep *Endpoint) string {
	t.Helper()
	a, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sip:%s@%s", ep.cfg.User, net.JoinHostPort(a.AdvertiseIP.String(), strconv.Itoa(a.AdvertiseSIPPort)))
}

func acceptOne(ep *Endpoint) <-chan *Call {
	ch := make(chan *Call, 1)
	go func() {
		c, err := ep.Accept()
		if err == nil {
			ch <- c
		}
	}()
	return ch
}

func readCallPCM(t *testing.T, call *Call, d time.Duration) []byte {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		pcm, err := call.ReadPCM()
		if err != nil {
			got <- nil
			return
		}
		got <- pcm
	}()
	select {
	case pcm := <-got:
		if len(pcm) == 0 {
			t.Fatal("ReadPCM failed")
		}
		return pcm
	case <-time.After(d):
		t.Fatal("ReadPCM timed out")
		return nil
	}
}

func earlyBodyString(port int, telephone bool) string {
	return string(earlyBody(port, telephone))
}

func challenge(realm, nonce string) string {
	return (&digest.Challenge{Realm: realm, Nonce: nonce, Algorithm: "MD5", QOP: []string{"auth"}}).String()
}

type uas struct {
	t     *testing.T
	conn  *net.UDPConn
	rtp   *net.UDPConn
	mu    sync.Mutex
	peer  *net.UDPAddr
	toTag string
	inbox []sipRaw
	seen  int
	wake  chan struct{}
}

func newUAS(t *testing.T) *uas {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	rtpSock := listenLoop(t)
	u := &uas{
		t:     t,
		conn:  conn,
		rtp:   rtpSock,
		toTag: randToken(),
		wake:  make(chan struct{}),
	}
	go u.loop()
	return u
}

func (u *uas) uri() string {
	return "sip:bob@" + u.conn.LocalAddr().String()
}

func (u *uas) rtpPort() int {
	return u.rtp.LocalAddr().(*net.UDPAddr).Port
}

func (u *uas) loop() {
	buf := make([]byte, 65535)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg, err := parseSIP(buf[:n])
		if err != nil {
			continue
		}
		if msg.method == "BYE" {
			u.respond(msg, 200, "OK", "", nil)
		}
		u.mu.Lock()
		u.peer = from
		u.inbox = append(u.inbox, msg)
		close(u.wake)
		u.wake = make(chan struct{})
		u.mu.Unlock()
	}
}

func (u *uas) respond(req sipRaw, code int, reason, body string, extra map[string]string) {
	u.t.Helper()
	u.mu.Lock()
	peer := u.peer
	tag := u.toTag
	u.mu.Unlock()
	if peer == nil {
		u.t.Fatal("response without a request")
	}
	to := req.hdr("to")
	if !strings.Contains(strings.ToLower(to), "tag=") {
		to += ";tag=" + tag
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", code, reason)
	for _, via := range req.headers["via"] {
		fmt.Fprintf(&b, "Via: %s\r\n", via)
	}
	fmt.Fprintf(&b, "From: %s\r\n", req.hdr("from"))
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", req.hdr("call-id"))
	fmt.Fprintf(&b, "CSeq: %s\r\n", req.hdr("cseq"))
	fmt.Fprintf(&b, "Contact: <sip:bob@%s>\r\n", u.conn.LocalAddr().String())
	for k, v := range extra {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" && extra["Content-Type"] == "" {
		b.WriteString("Content-Type: application/sdp\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)
	if _, err := u.conn.WriteToUDP([]byte(b.String()), peer); err != nil {
		u.t.Fatalf("respond %d: %v", code, err)
	}
}

func (u *uas) notify(refer sipRaw, body, state string) {
	u.t.Helper()
	u.mu.Lock()
	peer := u.peer
	u.mu.Unlock()
	contact := uriIn(refer.hdr("contact"))
	if contact == "" {
		contact = refer.hdr("from")
	}
	target := contact
	if i := strings.Index(target, ";"); i >= 0 && !strings.Contains(target, "tag=") {
		// keep transport params
	}
	var b strings.Builder
	fmt.Fprintf(&b, "NOTIFY %s SIP/2.0\r\n", uriIn(refer.hdr("contact")))
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP %s;branch=%s\r\n", u.conn.LocalAddr().String(), sip.GenerateBranch())
	fmt.Fprintf(&b, "From: %s\r\n", refer.hdr("to"))
	fmt.Fprintf(&b, "To: %s\r\n", refer.hdr("from"))
	fmt.Fprintf(&b, "Call-ID: %s\r\n", refer.hdr("call-id"))
	fmt.Fprintf(&b, "CSeq: 2 NOTIFY\r\n")
	fmt.Fprintf(&b, "Contact: <sip:bob@%s>\r\n", u.conn.LocalAddr().String())
	b.WriteString("Event: refer\r\n")
	fmt.Fprintf(&b, "Subscription-State: %s\r\n", state)
	b.WriteString("Content-Type: message/sipfrag;version=2.0\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)
	if _, err := u.conn.WriteToUDP([]byte(b.String()), peer); err != nil {
		u.t.Fatalf("notify: %v", err)
	}
}

func (u *uas) sendRTP(desc Description, payload []byte, seq uint16) {
	u.t.Helper()
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    0,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * frameSamples,
			SSRC:           0x22222222,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		u.t.Fatal(err)
	}
	dst := &net.UDPAddr{IP: desc.Connection, Port: desc.RTPPort}
	if _, err := u.rtp.WriteToUDP(raw, dst); err != nil {
		u.t.Fatal(err)
	}
}

func (u *uas) waitMethod(method string, d time.Duration) sipRaw {
	u.t.Helper()
	return u.next(d, func(m sipRaw) bool { return m.method == method })
}

func (u *uas) waitCode(code int, method string, d time.Duration) sipRaw {
	u.t.Helper()
	return u.next(d, func(m sipRaw) bool {
		_, got := m.cseq()
		return m.code == code && got == method
	})
}

func (u *uas) peekMethod(method string, d time.Duration) (sipRaw, bool) {
	u.t.Helper()
	deadline := time.Now().Add(d)
	for {
		u.mu.Lock()
		for u.seen < len(u.inbox) {
			msg := u.inbox[u.seen]
			u.seen++
			if msg.method == method {
				u.mu.Unlock()
				return msg, true
			}
		}
		wake := u.wake
		u.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return sipRaw{}, false
		}
	}
}

func (u *uas) next(d time.Duration, match func(sipRaw) bool) sipRaw {
	u.t.Helper()
	deadline := time.Now().Add(d)
	for {
		u.mu.Lock()
		for u.seen < len(u.inbox) {
			msg := u.inbox[u.seen]
			u.seen++
			if match(msg) {
				u.mu.Unlock()
				return msg
			}
		}
		wake := u.wake
		u.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			u.t.Fatal("sip timed out")
		}
	}
	return sipRaw{}
}
