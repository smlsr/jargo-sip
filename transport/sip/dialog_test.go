package sip

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
)

func TestLocalCSeqIsPerDirection(t *testing.T) {
	d := &dialog{remoteCSeq: 20, remoteInvite: 20, haveRemote: true}
	bye, err := d.nextCSeq(sip.BYE)
	if err != nil || bye != 1 {
		t.Fatalf("bye cseq %d %v", bye, err)
	}
	invite, err := d.nextCSeq(sip.INVITE)
	if err != nil || invite != 2 || d.localInvite != 2 {
		t.Fatalf("invite cseq %d local %d", invite, d.localInvite)
	}
	ack, err := d.nextCSeq(sip.ACK)
	if err != nil || ack != 2 {
		t.Fatalf("ack cseq %d %v", ack, err)
	}
	cancel, err := d.nextCSeq(sip.CANCEL)
	if err != nil || cancel != 2 {
		t.Fatalf("cancel cseq %d %v", cancel, err)
	}
	next, err := d.nextCSeq(sip.UPDATE)
	if err != nil || next != 3 {
		t.Fatalf("update cseq %d %v", next, err)
	}

	ackReq := sip.NewRequest(sip.ACK, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	ackReq.AppendHeader(&sip.CSeqHeader{SeqNo: 20, MethodName: sip.ACK})
	if err := d.noteRemote(ackReq); err != nil || d.remoteCSeq != 20 {
		t.Fatalf("ack note %v remote %d", err, d.remoteCSeq)
	}
	low := sip.NewRequest(sip.BYE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	low.AppendHeader(&sip.CSeqHeader{SeqNo: 20, MethodName: sip.BYE})
	if err := d.noteRemote(low); err == nil {
		t.Fatal("equal bye was accepted")
	}
	cancelReq := sip.NewRequest(sip.CANCEL, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	cancelReq.AppendHeader(&sip.CSeqHeader{SeqNo: 20, MethodName: sip.CANCEL})
	if err := d.noteRemote(cancelReq); err != nil || d.remoteCSeq != 20 {
		t.Fatalf("cancel note %v remote %d", err, d.remoteCSeq)
	}
	higher := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	higher.AppendHeader(&sip.CSeqHeader{SeqNo: 21, MethodName: sip.INVITE})
	if err := d.noteRemote(higher); err != nil || d.remoteCSeq != 21 || d.remoteInvite != 21 {
		t.Fatalf("reinvite note %v remote %d invite %d", err, d.remoteCSeq, d.remoteInvite)
	}
}

func TestGlareDelayFollowsCallIDOwner(t *testing.T) {
	for i := 0; i < 40; i++ {
		owner := glareDelay(true)
		if owner < 2100*time.Millisecond || owner > 4000*time.Millisecond {
			t.Fatalf("owner wait %s", owner)
		}
		other := glareDelay(false)
		if other < 0 || other > 2000*time.Millisecond {
			t.Fatalf("other wait %s", other)
		}
		retry := randRetryAfter()
		if retry < 0 || retry > 10 {
			t.Fatalf("retry-after %d", retry)
		}
	}
}

func TestInboundInviteThenAckStartsMedia(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	accepted := make(chan *Call, 1)
	go func() {
		c, err := ep.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		accepted <- c
	}()

	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	trying := leg.waitCode(100, "INVITE", 2*time.Second)
	if trying.hdr("rseq") != "" || strings.Contains(strings.ToLower(trying.hdr("require")), "100rel") {
		t.Fatalf("100 is reliable:\n%s", trying.raw)
	}
	assertCommon(t, trying)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	assertCommon(t, ok)
	if !strings.Contains(ok.body, "m=audio") || !strings.Contains(ok.body, "PCMU/8000") {
		t.Fatalf("sdp\n%s", ok.body)
	}
	desc, err := Parse(ok.body)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Version != 1 {
		t.Fatalf("answer version %d", desc.Version)
	}
	select {
	case <-accepted:
		t.Fatal("Accept returned before ACK")
	case <-time.After(200 * time.Millisecond):
	}
	if pkt := leg.tryRTP(200 * time.Millisecond); pkt != nil {
		t.Fatal("RTP arrived before ACK")
	}
	leg.ack(ok)
	var call *Call
	select {
	case call = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after ACK")
	}
	pkt := leg.readRTP(time.Second)
	if pkt.PayloadType != 0 || !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatalf("first media pt %d len %d", pkt.PayloadType, len(pkt.Payload))
	}
	party := call.Remote()
	if party.CallID != leg.callID || party.RemoteTag != leg.fromTag || party.LocalTag == "" {
		t.Fatalf("party %+v", party)
	}
	if !strings.Contains(party.RemoteTarget, "127.0.0.1") {
		t.Fatalf("target %s", party.RemoteTarget)
	}
	leg.expectByeCSeq(call, 1)
}

func TestAllowFromDropsInvite(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowFrom = []string{"10.9.8.7"}
	ep := startEP(t, cfg)
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	if msg, ok := leg.peek(400 * time.Millisecond); ok {
		t.Fatalf("disallowed source got %s", msg.first)
	}
}

func TestDigestChallengesOnce(t *testing.T) {
	cfg := baseConfig()
	cfg.RequireAuth = true
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	ep := startEP(t, cfg)
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	unauth := leg.waitCode(401, "INVITE", 2*time.Second)
	if !strings.Contains(unauth.hdr("www-authenticate"), `realm="sip.example.com"`) {
		t.Fatalf("challenge %q", unauth.hdr("www-authenticate"))
	}
	assertCommon(t, unauth)
	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{
		"Authorization": leg.authorization(unauth, "s3cret"),
	})
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if ok.hdr("www-authenticate") != "" {
		t.Fatalf("second challenge %q", ok.hdr("www-authenticate"))
	}
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestDigestBadCredentialsForbidden(t *testing.T) {
	cfg := baseConfig()
	cfg.RequireAuth = true
	cfg.Auth = &DigestAuth{Username: "bot", Password: "s3cret"}
	ep := startEP(t, cfg)
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	unauth := leg.waitCode(401, "INVITE", 2*time.Second)
	leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{
		"Authorization": leg.authorization(unauth, "wrong"),
	})
	forbidden := leg.waitCode(403, "INVITE", 2*time.Second)
	if forbidden.hdr("www-authenticate") != "" {
		t.Fatalf("403 carried a challenge %q", forbidden.hdr("www-authenticate"))
	}
	assertCommon(t, forbidden)
	errc := make(chan error, 1)
	go func() {
		_, err := ep.Accept()
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Accept returned a call")
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestUnsupportedCodec488FreesPort(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	leg.invite(codecOffer(leg.rtpPort(), "9", "a=rtpmap:9 G722/8000", "sendrecv"), false, nil)
	leg.waitCode(488, "INVITE", 2*time.Second)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	desc, err := Parse(ok.body)
	if err != nil {
		t.Fatal(err)
	}
	if desc.RTPPort != ep.cfg.RTPPortMin {
		t.Fatalf("rtp port %d, want %d", desc.RTPPort, ep.cfg.RTPPortMin)
	}
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestCancelBefore200DropsCall(t *testing.T) {
	ep := startEP(t, baseConfig())
	release := make(chan struct{})
	ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
		if !reinviteOf(req) {
			<-release
		}
	})
	leg := newLeg(t, ep)
	accepted := make(chan *Call, 1)
	go func() {
		c, err := ep.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	leg.waitCode(100, "INVITE", 2*time.Second)
	leg.cancel()
	leg.waitCode(487, "INVITE", 2*time.Second)
	close(release)
	select {
	case <-accepted:
		t.Fatal("CANCEL delivered a call")
	case <-time.After(300 * time.Millisecond):
	}
	time.Sleep(50 * time.Millisecond)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	desc, err := Parse(ok.body)
	if err != nil {
		t.Fatal(err)
	}
	if desc.RTPPort != ep.cfg.RTPPortMin {
		t.Fatalf("port %d was not released", desc.RTPPort)
	}
	leg.ack(ok)
	var call *Call
	select {
	case call = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return the next call")
	}
	_ = call.Hangup(context.Background())
}

func TestByeClosesCall(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	leg.bye()
	ok := leg.waitCode(200, "BYE", 2*time.Second)
	assertCommon(t, ok)
	select {
	case <-call.Done():
	case <-time.After(time.Second):
		t.Fatal("Done stayed open")
	}
	if _, err := call.ReadPCM(); err == nil {
		t.Fatal("ReadPCM after BYE")
	}
}

func TestCloseUnblocksAccept(t *testing.T) {
	ep := startEP(t, baseConfig())
	errc := make(chan error, 1)
	go func() {
		_, err := ep.Accept()
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if err := ep.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrEndpointClosed) {
			t.Fatalf("Accept err %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept stayed blocked")
	}
}

func TestConcurrentAcceptsTakeDifferentCalls(t *testing.T) {
	ep := startEP(t, baseConfig())
	var wg sync.WaitGroup
	got := make(chan *Call, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := ep.Accept()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			got <- c
		}()
	}
	a := newLeg(t, ep)
	b := newLeg(t, ep)
	a.invite(pcmuOffer(a.rtpPort()), false, nil)
	b.invite(pcmuOffer(b.rtpPort()), false, nil)
	a.ack(a.waitCode(200, "INVITE", 2*time.Second))
	b.ack(b.waitCode(200, "INVITE", 2*time.Second))
	var calls []*Call
	timeout := time.After(2 * time.Second)
	for len(calls) < 2 {
		select {
		case c := <-got:
			calls = append(calls, c)
		case <-timeout:
			t.Fatal("timed out waiting for both calls")
		}
	}
	wg.Wait()
	if calls[0].Remote().CallID == calls[1].Remote().CallID {
		t.Fatal("both Accept callers got the same call")
	}
	_ = calls[0].Hangup(context.Background())
	_ = calls[1].Hangup(context.Background())
}

func TestReinviteHoldCommitsBeforeAck(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	leg.mustHearCaller(t, call)
	before := leg.readRTP(time.Second)

	leg.inDialog("INVITE", pcmuDir(leg.rtpPort(), "sendonly"), leg.cseq+1)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if !strings.Contains(ok.body, "a=sendonly") {
		t.Fatalf("answer direction\n%s", ok.body)
	}
	leg.mustConceal(t, call)
	pkt := leg.readRTP(time.Second)
	if !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatal("sender was not silence after the 200")
	}
	if pkt.SSRC != before.SSRC || seqDelta(before.SequenceNumber, pkt.SequenceNumber) > 25 {
		t.Fatalf("sequence reset %d -> %d ssrc %d %d", before.SequenceNumber, pkt.SequenceNumber, before.SSRC, pkt.SSRC)
	}
	leg.ack(ok)
	_ = call.Hangup(context.Background())
}

func TestUpdateHoldAndResume(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, true)
	leg.mustHearCaller(t, call)
	before := leg.readRTP(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- call.Hold(ctx) }()
	upd := leg.waitMethod("UPDATE", 2*time.Second)
	if !strings.Contains(upd.body, "a=sendonly") {
		t.Fatalf("hold offer\n%s", upd.body)
	}
	if _, method := upd.cseq(); method != "UPDATE" {
		t.Fatalf("method %s", method)
	}
	seq, _ := upd.cseq()
	if seq != 1 {
		t.Fatalf("first local cseq %d", seq)
	}
	leg.respond(upd, 200, "OK", pcmuDir(leg.rtpPort(), "recvonly"))
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if ack, ok := leg.peekMethod("ACK", 200*time.Millisecond); ok {
		t.Fatalf("UPDATE was acked\n%s", ack.raw)
	}
	leg.mustConceal(t, call)
	pkt := leg.readRTP(time.Second)
	if pkt.SSRC != before.SSRC || !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatal("update hold did not keep silence on the same ssrc")
	}

	go func() { errc <- call.Resume(ctx) }()
	resume := leg.waitMethod("UPDATE", 2*time.Second)
	if !strings.Contains(resume.body, "a=sendrecv") {
		t.Fatalf("resume offer\n%s", resume.body)
	}
	leg.respond(resume, 200, "OK", pcmuDir(leg.rtpPort(), "sendrecv"))
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestUpdatePortResetsLatch(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, true)
	desc, err := Parse(leg.lastOK.body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		leg.sendAudio(desc, 0x00, uint16(i+1))
	}
	waitFor(t, time.Second, func() bool {
		return call.dlg.media.RTPLatch() == LatchCommitted
	})
	next := listenLoop(t)
	leg.inDialog("UPDATE", pcmuDir(next.LocalAddr().(*net.UDPAddr).Port, "sendrecv"), leg.cseq+1)
	ok := leg.waitCode(200, "UPDATE", 2*time.Second)
	leg.noteTo(ok)
	if call.dlg.media.RTPLatch() != LatchSDPTarget {
		t.Fatalf("latch %s", call.dlg.media.RTPLatch())
	}
	if call.dlg.media.RTPDest().Port != next.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("dest %s", call.dlg.media.RTPDest())
	}
	if call.dlg.media.Stats().SSRC == 0 {
		t.Fatal("ssrc was cleared")
	}
	_ = call.Hangup(context.Background())
}

func TestReinviteWithoutSDPKeepsMedia(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	desc, err := Parse(leg.lastOK.body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		leg.sendAudio(desc, 0x00, uint16(i+1))
	}
	waitFor(t, time.Second, func() bool {
		return call.dlg.media.RTPLatch() == LatchCommitted
	})
	dest := call.dlg.media.RTPDest().Port
	leg.inDialog("INVITE", "", leg.cseq+1)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if !strings.Contains(ok.body, fmt.Sprintf("m=audio %d ", desc.RTPPort)) {
		t.Fatalf("sdp\n%s", ok.body)
	}
	if call.dlg.media.RTPLatch() != LatchCommitted || call.dlg.media.RTPDest().Port != dest {
		t.Fatalf("media moved latch %s dest %d", call.dlg.media.RTPLatch(), call.dlg.media.RTPDest().Port)
	}
	leg.ack(ok)
	_ = call.Hangup(context.Background())
}

func TestReinviteUnknownCodecKeepsSession(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	before := leg.readRTP(time.Second)
	leg.inDialog("INVITE", codecOffer(leg.rtpPort(), "9", "a=rtpmap:9 G722/8000", "sendrecv"), leg.cseq+1)
	leg.waitCode(488, "INVITE", 2*time.Second)
	pkt := leg.readRTP(time.Second)
	if pkt.SSRC != before.SSRC || seqDelta(before.SequenceNumber, pkt.SequenceNumber) > 25 {
		t.Fatal("488 replaced the session")
	}
	_ = call.Hangup(context.Background())
}

func TestUpdateWaitsUntilAck(t *testing.T) {
	ep := startEP(t, baseConfig())
	release := make(chan struct{})
	ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
		if reinviteOf(req) {
			<-release
		}
	})
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, true)
	leg.inDialog("INVITE", pcmuDir(leg.rtpPort(), "sendrecv"), leg.cseq+1)
	time.Sleep(50 * time.Millisecond)
	errc := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { errc <- call.Hold(ctx) }()
	if msg, ok := leg.peekMethod("UPDATE", 200*time.Millisecond); ok {
		t.Fatalf("UPDATE left while the INVITE was open\n%s", msg.raw)
	}
	if msg, ok := leg.peekMethod("INVITE", 50*time.Millisecond); ok && strings.Contains(msg.body, "a=sendonly") {
		t.Fatalf("re-INVITE left while the offer was open\n%s", msg.raw)
	}
	close(release)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if msg, okm := leg.peekMethod("UPDATE", 150*time.Millisecond); okm {
		t.Fatalf("UPDATE left before ACK\n%s", msg.raw)
	}
	leg.ack(ok)
	upd := leg.waitMethod("UPDATE", 2*time.Second)
	leg.respond(upd, 200, "OK", pcmuDir(leg.rtpPort(), "recvonly"))
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func TestSecondInviteWhileUnansweredIs500(t *testing.T) {
	ep := startEP(t, baseConfig())
	release := make(chan struct{})
	ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
		if reinviteOf(req) {
			<-release
		}
	})
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	leg.inDialog("INVITE", pcmuDir(leg.rtpPort(), "sendonly"), leg.cseq+1)
	time.Sleep(30 * time.Millisecond)
	leg.inDialog("INVITE", pcmuDir(leg.rtpPort(), "sendrecv"), leg.cseq+1)
	fail := leg.waitCode(500, "INVITE", 2*time.Second)
	retry := fail.hdr("retry-after")
	n, err := strconv.Atoi(strings.TrimSpace(retry))
	if err != nil || n < 0 || n > 10 {
		t.Fatalf("Retry-After %q", retry)
	}
	assertCommon(t, fail)
	close(release)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	leg.ack(ok)
	_ = call.Hangup(context.Background())
}

func TestCrossingReinviteRetriesOnce(t *testing.T) {
	ep := startEP(t, baseConfig())
	leg := newLeg(t, ep)
	call := establish(t, ep, leg, false)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- call.Hold(ctx) }()

	first := leg.waitMethod("INVITE", 2*time.Second)
	firstSeq, _ := first.cseq()
	if firstSeq != 1 || !strings.Contains(first.body, "a=sendonly") {
		t.Fatalf("first offer cseq %d\n%s", firstSeq, first.body)
	}
	firstDesc, err := Parse(first.body)
	if err != nil {
		t.Fatal(err)
	}
	leg.inDialog("INVITE", pcmuDir(leg.rtpPort(), "sendonly"), leg.cseq+1)
	peer := leg.waitCode(491, "INVITE", 2*time.Second)
	if _, method := peer.cseq(); method != "INVITE" {
		t.Fatalf("491 for %s", method)
	}
	leg.respond(first, 491, "Request Pending", "")
	second := leg.waitMethod("INVITE", 5*time.Second)
	secondSeq, _ := second.cseq()
	if secondSeq == firstSeq {
		t.Fatal("retry reused the CSeq")
	}
	secondDesc, err := Parse(second.body)
	if err != nil {
		t.Fatal(err)
	}
	if secondDesc.Version != firstDesc.Version+1 {
		t.Fatalf("offer version %d then %d", firstDesc.Version, secondDesc.Version)
	}
	leg.respond(second, 200, "OK", pcmuDir(leg.rtpPort(), "recvonly"))
	ack := leg.waitMethod("ACK", 2*time.Second)
	ackSeq, ackMethod := ack.cseq()
	if ackMethod != "ACK" || ackSeq != secondSeq {
		t.Fatalf("ack cseq %d %s, invite %d", ackSeq, ackMethod, secondSeq)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, ok := leg.peekMethod("INVITE", 50*time.Millisecond)
		if !ok {
			break
		}
		seq, _ := msg.cseq()
		if seq != firstSeq && seq != secondSeq {
			t.Fatalf("extra invite cseq %d", seq)
		}
	}
	_ = call.Hangup(context.Background())
}

func TestReliableProvisional(t *testing.T) {
	t.Run("unreliable", func(t *testing.T) {
		ep := startEP(t, baseConfig())
		ep.setBeforeFinal(func(d *dialog, req *sip.Request) {
			if reinviteOf(req) {
				return
			}
			if err := d.sendProvisional(180, "Ringing"); err != nil {
				t.Errorf("180: %v", err)
			}
			time.Sleep(800 * time.Millisecond)
		})
		leg := newLeg(t, ep)
		leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
		ok := leg.waitCode(200, "INVITE", 3*time.Second)
		if n := leg.countCode(180, "INVITE"); n != 1 {
			t.Fatalf("180 count %d", n)
		}
		ring := leg.findCode(180, "INVITE")
		if ring.hdr("rseq") != "" || strings.Contains(strings.ToLower(ring.hdr("require")), "100rel") {
			t.Fatalf("unreliable 180\n%s", ring.raw)
		}
		leg.ack(ok)
		call, err := ep.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_ = call.Hangup(context.Background())
	})

	t.Run("supported", func(t *testing.T) {
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
		leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{"Supported": "100rel"})
		waitFor(t, 3*time.Second, func() bool {
			return leg.countCode(180, "INVITE") >= 2
		})
		ring := leg.findCode(180, "INVITE")
		if !strings.EqualFold(strings.TrimSpace(ring.hdr("require")), "100rel") || ring.hdr("rseq") == "" {
			t.Fatalf("reliable 180\n%s", ring.raw)
		}
		trying := leg.findCode(100, "INVITE")
		if trying.hdr("rseq") != "" {
			t.Fatalf("100 carried RSeq\n%s", trying.raw)
		}
		rseq, err := strconv.Atoi(strings.TrimSpace(ring.hdr("rseq")))
		if err != nil || rseq < 1 {
			t.Fatalf("rseq %q", ring.hdr("rseq"))
		}
		inviteSeq, _ := ring.cseq()
		leg.prack(inviteSeq+1, fmt.Sprintf("%d %d INVITE", rseq+9, inviteSeq))
		bad := leg.waitCode(481, "PRACK", 2*time.Second)
		assertCommon(t, bad)
		n := leg.countCode(180, "INVITE")
		waitFor(t, 2*time.Second, func() bool {
			return leg.countCode(180, "INVITE") > n
		})
		leg.prack(inviteSeq+2, fmt.Sprintf("%d %d INVITE", rseq, inviteSeq))
		leg.waitCode(200, "PRACK", 2*time.Second)
		stable := leg.countCode(180, "INVITE")
		time.Sleep(1300 * time.Millisecond)
		if got := leg.countCode(180, "INVITE"); got != stable {
			t.Fatalf("180 kept retransmitting %d -> %d", stable, got)
		}
		close(release)
		ok := leg.waitCode(200, "INVITE", 2*time.Second)
		leg.ack(ok)
		call, err := ep.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_ = call.Hangup(context.Background())
	})

	t.Run("require without 18x", func(t *testing.T) {
		ep := startEP(t, baseConfig())
		leg := newLeg(t, ep)
		leg.invite(pcmuOffer(leg.rtpPort()), false, map[string]string{"Require": "100rel"})
		ok := leg.waitCode(200, "INVITE", 2*time.Second)
		if leg.countCode(180, "INVITE") != 0 {
			t.Fatal("unexpected 180")
		}
		if !strings.Contains(ok.hdr("supported"), "100rel") {
			t.Fatalf("supported %q", ok.hdr("supported"))
		}
		leg.ack(ok)
		call, err := ep.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_ = call.Hangup(context.Background())
	})
}

func TestSupportedTimerWhenConfigured(t *testing.T) {
	cfg := baseConfig()
	cfg.SessionTimer = time.Minute
	ep := startEP(t, cfg)
	leg := newLeg(t, ep)
	leg.invite(pcmuOffer(leg.rtpPort()), false, nil)
	ok := leg.waitCode(200, "INVITE", 2*time.Second)
	if !strings.Contains(ok.hdr("supported"), "100rel") || !strings.Contains(ok.hdr("supported"), "timer") {
		t.Fatalf("supported %q", ok.hdr("supported"))
	}
	leg.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = call.Hangup(context.Background())
}

func assertCommon(t *testing.T, msg sipRaw) {
	t.Helper()
	if msg.hdr("allow") != allowMethods {
		t.Fatalf("Allow %q", msg.hdr("allow"))
	}
	if !strings.Contains(msg.hdr("supported"), "100rel") {
		t.Fatalf("Supported %q", msg.hdr("supported"))
	}
	if !strings.Contains(msg.hdr("contact"), "sip:bot@") {
		t.Fatalf("Contact %q", msg.hdr("contact"))
	}
}

func reinviteOf(req *sip.Request) bool {
	to := req.To()
	if to == nil {
		return false
	}
	tag, ok := to.Params.Get("tag")
	return ok && tag != ""
}

func seqDelta(prev, next uint16) uint16 {
	return next - prev
}

func pcmuOffer(port int) string {
	return pcmuDir(port, "sendrecv")
}

func pcmuDir(port int, dir string) string {
	return codecOffer(port, "0 101", "a=rtpmap:0 PCMU/8000\na=rtpmap:101 telephone-event/8000\na=fmtp:101 0-15", dir)
}

func codecOffer(port int, payloads, maps, dir string) string {
	return crlf(fmt.Sprintf("v=0\no=- 1 1 IN IP4 127.0.0.1\ns=-\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio %d RTP/AVP %s\n%s\na=ptime:20\na=%s\n", port, payloads, maps, dir))
}

type sipRaw struct {
	raw     string
	first   string
	code    int
	method  string
	headers map[string][]string
	body    string
}

func (m sipRaw) hdr(name string) string {
	vals := m.headers[strings.ToLower(name)]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (m sipRaw) cseq() (uint32, string) {
	fields := strings.Fields(m.hdr("cseq"))
	if len(fields) != 2 {
		return 0, ""
	}
	n, _ := strconv.ParseUint(fields[0], 10, 32)
	return uint32(n), strings.ToUpper(fields[1])
}

func parseSIP(raw []byte) (sipRaw, error) {
	text := strings.TrimRight(string(raw), "\x00")
	parts := strings.SplitN(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n", 2)
	lines := strings.Split(parts[0], "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return sipRaw{}, errors.New("empty sip message")
	}
	msg := sipRaw{
		raw:     text,
		first:   strings.TrimSpace(lines[0]),
		headers: map[string][]string{},
	}
	if strings.HasPrefix(msg.first, "SIP/2.0") {
		fields := strings.SplitN(msg.first, " ", 3)
		if len(fields) < 2 {
			return sipRaw{}, errors.New("bad status")
		}
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			return sipRaw{}, err
		}
		msg.code = code
	} else {
		fields := strings.SplitN(msg.first, " ", 2)
		msg.method = strings.ToUpper(fields[0])
	}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		msg.headers[key] = append(msg.headers[key], strings.TrimSpace(value))
	}
	if len(parts) == 2 {
		msg.body = parts[1]
		if n, err := strconv.Atoi(msg.hdr("content-length")); err == nil && n >= 0 && n < len(msg.body) {
			msg.body = msg.body[:n]
		}
	}
	return msg, nil
}

type leg struct {
	t       *testing.T
	conn    *net.UDPConn
	rtp     *net.UDPConn
	dest    *net.UDPAddr
	callID  string
	fromTag string
	toTag   string
	cseq    uint32
	branch  string
	request string
	lastOK  sipRaw
	mu      sync.Mutex
	inbox   []sipRaw
	seen    int
	wake    chan struct{}
}

func newLeg(t *testing.T, ep *Endpoint) *leg {
	t.Helper()
	addrs, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	rtpSock := listenLoop(t)
	l := &leg{
		t:       t,
		conn:    conn,
		rtp:     rtpSock,
		dest:    &net.UDPAddr{IP: addrs.AdvertiseIP, Port: addrs.AdvertiseSIPPort},
		callID:  randToken() + "@127.0.0.1",
		fromTag: randToken(),
		cseq:    19,
		request: fmt.Sprintf("sip:bot@%s", net.JoinHostPort(addrs.AdvertiseIP.String(), strconv.Itoa(addrs.AdvertiseSIPPort))),
		wake:    make(chan struct{}),
	}
	go l.loop()
	return l
}

func (l *leg) loop() {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg, err := parseSIP(buf[:n])
		if err != nil {
			continue
		}
		l.noteTo(msg)
		if msg.code >= 300 {
			_, method := msg.cseq()
			if method == "INVITE" {
				l.ack(msg)
			}
		}
		if msg.method == "BYE" {
			l.respond(msg, 200, "OK", "")
		}
		l.mu.Lock()
		l.inbox = append(l.inbox, msg)
		close(l.wake)
		l.wake = make(chan struct{})
		l.mu.Unlock()
	}
}

func (l *leg) noteTo(msg sipRaw) {
	if l.toTag != "" {
		return
	}
	if tag := headerTag(msg.hdr("to")); tag != "" {
		l.toTag = tag
	}
}

func (l *leg) rtpPort() int {
	return l.rtp.LocalAddr().(*net.UDPAddr).Port
}

func (l *leg) srcPort() int {
	return l.conn.LocalAddr().(*net.UDPAddr).Port
}

func (l *leg) invite(body string, allowUpdate bool, extra map[string]string) {
	l.t.Helper()
	l.cseq = 20
	l.branch = sip.GenerateBranch()
	headers := map[string]string{}
	if allowUpdate {
		headers["Allow"] = "INVITE, ACK, CANCEL, BYE, UPDATE"
	}
	for k, v := range extra {
		headers[k] = v
	}
	l.send("INVITE", l.request, l.branch, l.cseq, "INVITE", body, headers, false)
}

func (l *leg) cancel() {
	l.t.Helper()
	l.send("CANCEL", l.request, l.branch, 20, "CANCEL", "", nil, false)
}

func (l *leg) bye() {
	l.t.Helper()
	l.cseq++
	l.send("BYE", l.request, sip.GenerateBranch(), l.cseq, "BYE", "", nil, true)
}

func (l *leg) inDialog(method, body string, cseq uint32) {
	l.t.Helper()
	if cseq > l.cseq {
		l.cseq = cseq
	}
	target := l.request
	l.send(method, target, sip.GenerateBranch(), cseq, method, body, nil, true)
}

func (l *leg) prack(cseq uint32, rack string) {
	l.t.Helper()
	if cseq > l.cseq {
		l.cseq = cseq
	}
	l.send("PRACK", l.request, sip.GenerateBranch(), cseq, "PRACK", "", map[string]string{"RAck": rack}, true)
}

func (l *leg) send(method, uri, branch string, cseq uint32, cseqMethod, body string, extra map[string]string, dialog bool) {
	l.t.Helper()
	from := fmt.Sprintf(`"Alice" <sip:alice@sip.example.com>;tag=%s`, l.fromTag)
	to := "<sip:bot@sip.example.com>"
	if dialog && l.toTag != "" {
		to = fmt.Sprintf("<sip:bot@sip.example.com>;tag=%s", l.toTag)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", method, uri)
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP 127.0.0.1:%d;branch=%s;rport\r\n", l.srcPort(), branch)
	fmt.Fprintf(&b, "Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", l.callID)
	fmt.Fprintf(&b, "CSeq: %d %s\r\n", cseq, cseqMethod)
	fmt.Fprintf(&b, "Contact: <sip:alice@127.0.0.1:%d>\r\n", l.srcPort())
	for k, v := range extra {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" {
		b.WriteString("Content-Type: application/sdp\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)
	if _, err := l.conn.WriteToUDP([]byte(b.String()), l.dest); err != nil {
		l.t.Fatalf("send %s: %v", method, err)
	}
}

func (l *leg) ack(res sipRaw) {
	if res.code == 0 {
		return
	}
	seq, method := res.cseq()
	if method != "INVITE" {
		return
	}
	uri := l.request
	if contact := res.hdr("contact"); contact != "" {
		if got := uriIn(contact); got != "" {
			uri = got
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ACK %s SIP/2.0\r\n", uri)
	for _, via := range res.headers["via"] {
		fmt.Fprintf(&b, "Via: %s\r\n", via)
	}
	if len(res.headers["via"]) == 0 {
		fmt.Fprintf(&b, "Via: SIP/2.0/UDP 127.0.0.1:%d;branch=%s;rport\r\n", l.srcPort(), l.branch)
	}
	fmt.Fprintf(&b, "Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: %s\r\n", res.hdr("from"))
	fmt.Fprintf(&b, "To: %s\r\n", res.hdr("to"))
	fmt.Fprintf(&b, "Call-ID: %s\r\n", res.hdr("call-id"))
	fmt.Fprintf(&b, "CSeq: %d ACK\r\n", seq)
	b.WriteString("Content-Length: 0\r\n\r\n")
	if _, err := l.conn.WriteToUDP([]byte(b.String()), l.dest); err != nil {
		l.t.Fatalf("ack: %v", err)
	}
}

func (l *leg) respond(req sipRaw, code int, reason, body string) {
	l.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", code, reason)
	for _, via := range req.headers["via"] {
		fmt.Fprintf(&b, "Via: %s\r\n", via)
	}
	fmt.Fprintf(&b, "From: %s\r\n", req.hdr("from"))
	fmt.Fprintf(&b, "To: %s\r\n", req.hdr("to"))
	fmt.Fprintf(&b, "Call-ID: %s\r\n", req.hdr("call-id"))
	fmt.Fprintf(&b, "CSeq: %s\r\n", req.hdr("cseq"))
	fmt.Fprintf(&b, "Contact: <sip:alice@127.0.0.1:%d>\r\n", l.srcPort())
	if body != "" {
		b.WriteString("Content-Type: application/sdp\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)
	if _, err := l.conn.WriteToUDP([]byte(b.String()), l.dest); err != nil {
		l.t.Fatalf("respond %d: %v", code, err)
	}
}

func (l *leg) authorization(challenge sipRaw, password string) string {
	l.t.Helper()
	chal, err := digest.ParseChallenge(challenge.hdr("www-authenticate"))
	if err != nil {
		l.t.Fatal(err)
	}
	cred, err := digest.Digest(chal, digest.Options{
		Method:   "INVITE",
		URI:      l.request,
		Username: "bot",
		Password: password,
	})
	if err != nil {
		l.t.Fatal(err)
	}
	return cred.String()
}

func (l *leg) waitCode(code int, method string, d time.Duration) sipRaw {
	l.t.Helper()
	msg := l.next(d, func(m sipRaw) bool {
		got, cseqMethod := m.cseq()
		_ = got
		return m.code == code && cseqMethod == method
	})
	if code == 200 && method == "INVITE" {
		l.lastOK = msg
	}
	return msg
}

func (l *leg) waitMethod(method string, d time.Duration) sipRaw {
	l.t.Helper()
	return l.next(d, func(m sipRaw) bool { return m.method == method })
}

func (l *leg) next(d time.Duration, match func(sipRaw) bool) sipRaw {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for {
		l.mu.Lock()
		for l.seen < len(l.inbox) {
			msg := l.inbox[l.seen]
			l.seen++
			if match(msg) {
				l.mu.Unlock()
				return msg
			}
		}
		wake := l.wake
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			timer.Stop()
			l.fail("timed out waiting for sip")
		}
	}
}

func (l *leg) peek(d time.Duration) (sipRaw, bool) {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for {
		l.mu.Lock()
		if l.seen < len(l.inbox) {
			msg := l.inbox[l.seen]
			l.seen++
			l.mu.Unlock()
			return msg, true
		}
		wake := l.wake
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return sipRaw{}, false
		}
	}
}

func (l *leg) peekMethod(method string, d time.Duration) (sipRaw, bool) {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for {
		l.mu.Lock()
		for l.seen < len(l.inbox) {
			msg := l.inbox[l.seen]
			l.seen++
			if msg.method == method {
				l.mu.Unlock()
				return msg, true
			}
		}
		wake := l.wake
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return sipRaw{}, false
		}
	}
}

func (l *leg) countCode(code int, method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, msg := range l.inbox {
		_, cseqMethod := msg.cseq()
		if msg.code == code && cseqMethod == method {
			n++
		}
	}
	return n
}

func (l *leg) findCode(code int, method string) sipRaw {
	l.t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, msg := range l.inbox {
		_, cseqMethod := msg.cseq()
		if msg.code == code && cseqMethod == method {
			return msg
		}
	}
	l.t.Fatalf("missing %d %s", code, method)
	return sipRaw{}
}

func (l *leg) fail(why string) {
	l.t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for i, msg := range l.inbox {
		fmt.Fprintf(&b, "--- %d %s\n", i, msg.first)
	}
	l.t.Fatalf("%s\n%s", why, b.String())
}

func (l *leg) tryRTP(d time.Duration) *rtp.Packet {
	buf := make([]byte, 2048)
	_ = l.rtp.SetReadDeadline(time.Now().Add(d))
	n, _, err := l.rtp.ReadFromUDP(buf)
	if err != nil {
		return nil
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		return nil
	}
	return pkt
}

func (l *leg) readRTP(d time.Duration) *rtp.Packet {
	l.t.Helper()
	if pkt := l.tryRTP(d); pkt != nil {
		return pkt
	}
	l.t.Fatal("rtp timed out")
	return nil
}

func (l *leg) sendAudio(desc Description, payload byte, seq uint16) {
	l.t.Helper()
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    0,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * frameSamples,
			SSRC:           0x11111111,
		},
		Payload: bytes.Repeat([]byte{payload}, frameSamples),
	}
	raw, err := pkt.Marshal()
	if err != nil {
		l.t.Fatal(err)
	}
	dst := &net.UDPAddr{IP: desc.Connection, Port: desc.RTPPort}
	if _, err := l.rtp.WriteToUDP(raw, dst); err != nil {
		l.t.Fatal(err)
	}
}

func (l *leg) mustHearCaller(t *testing.T, call *Call) {
	t.Helper()
	desc, err := Parse(l.lastOK.body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		l.sendAudio(desc, 0x00, uint16(i+1))
	}
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
		if len(pcm) == 0 || bytes.Equal(pcm, concealmentPCM()) {
			t.Fatal("ReadPCM did not return caller audio")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPCM did not return caller audio")
	}
}

func (l *leg) mustConceal(t *testing.T, call *Call) {
	t.Helper()
	desc, err := Parse(l.lastOK.body)
	if err != nil {
		t.Fatal(err)
	}
	l.sendAudio(desc, 0x00, 40)
	time.Sleep(30 * time.Millisecond)
	pcm, err := call.ReadPCM()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pcm, concealmentPCM()) {
		t.Fatal("ReadPCM returned caller audio")
	}
}

func (l *leg) expectByeCSeq(call *Call, want uint32) {
	l.t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- call.Hangup(context.Background()) }()
	bye := l.waitMethod("BYE", 2*time.Second)
	seq, method := bye.cseq()
	if method != "BYE" || seq != want {
		l.t.Fatalf("bye cseq %d %s", seq, method)
	}
	if err := <-errc; err != nil {
		l.t.Fatal(err)
	}
}

func establish(t *testing.T, ep *Endpoint, l *leg, allowUpdate bool) *Call {
	t.Helper()
	l.invite(pcmuOffer(l.rtpPort()), allowUpdate, nil)
	ok := l.waitCode(200, "INVITE", 3*time.Second)
	assertCommon(t, ok)
	l.ack(ok)
	call, err := ep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func headerTag(value string) string {
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "tag=") {
			return strings.TrimSpace(part[4:])
		}
	}
	return ""
}

func uriIn(contact string) string {
	start := strings.Index(contact, "<")
	end := strings.Index(contact, ">")
	if start >= 0 && end > start {
		return contact[start+1 : end]
	}
	return strings.TrimSpace(contact)
}

func randToken() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
