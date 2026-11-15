package sip

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func (e *Endpoint) openSIP(advIP net.IP, advPort int, udp *net.UDPConn, tcp *net.TCPListener, tlsLn net.Listener) error {
	if udp == nil && tlsLn == nil {
		return nil
	}
	local := net.Addr(nil)
	transportName := "udp"
	if udp != nil {
		local = udp.LocalAddr()
	} else {
		local = tlsLn.Addr()
		transportName = "tls"
	}
	host, port, err := sip.ParseAddr(local.String())
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("sip listen address is missing")
	}
	e.listen = sip.Addr{IP: append(net.IP(nil), ip...), Port: port, Hostname: ip.String()}
	params := sip.NewParams()
	params.Add("transport", transportName)
	e.contact = sip.ContactHeader{
		Address: sip.Uri{
			Scheme:    "sip",
			User:      e.cfg.User,
			Host:      advIP.String(),
			Port:      advPort,
			UriParams: params,
		},
	}
	e.tlsClient = clientTLSConfig(e.cfg.TLS)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("jargo-sip"),
		sipgo.WithUserAgentHostname(advIP.String()),
		sipgo.WithUserAgenTLSConfig(e.tlsClient),
	)
	if err != nil {
		return err
	}
	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientHostname(advIP.String()),
		sipgo.WithClientPort(advPort),
	}
	if udp != nil {
		clientOpts = append(clientOpts, sipgo.WithClientConnectionAddr(e.listen.String()))
	}
	cli, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		_ = ua.Close()
		return err
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		return err
	}
	e.sipUA = ua
	e.sipCli = cli
	srv.OnInvite(e.onInvite)
	srv.OnAck(e.onAck)
	srv.OnBye(e.onBye)
	srv.OnUpdate(e.onUpdate)
	srv.OnPrack(e.onPrack)
	srv.OnCancel(e.onCancel)
	srv.OnRefer(e.onRefer)
	srv.OnNotify(e.onNotify)
	srv.OnOptions(e.onOptions)
	if udp != nil {
		e.serveWG.Add(1)
		go func() {
			defer e.serveWG.Done()
			_ = srv.ServeUDP(udp)
		}()
	}
	if tcp != nil {
		e.serveWG.Add(1)
		go func() {
			defer e.serveWG.Done()
			_ = srv.ServeTCP(tcp)
		}()
	}
	if tlsLn != nil {
		e.serveWG.Add(1)
		go func() {
			defer e.serveWG.Done()
			_ = srv.ServeTLS(tlsLn)
		}()
	}
	return nil
}

func (e *Endpoint) dialogUA() *sipgo.DialogUA {
	return &sipgo.DialogUA{Client: e.sipCli, ContactHDR: e.contact}
}

func (e *Endpoint) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed || !e.started
}

func (e *Endpoint) Accept() (*Call, error) {
	for {
		e.acceptMu.Lock()
		if len(e.accepted) > 0 {
			c := e.accepted[0]
			e.accepted[0] = nil
			e.accepted = e.accepted[1:]
			e.acceptMu.Unlock()
			return c, nil
		}
		if e.acceptClosed {
			e.acceptMu.Unlock()
			return nil, ErrEndpointClosed
		}
		notify := e.acceptNotify
		e.acceptMu.Unlock()
		<-notify
	}
}

func (e *Endpoint) deliver(c *Call) {
	e.acceptMu.Lock()
	defer e.acceptMu.Unlock()
	if e.acceptClosed {
		c.finish()
		return
	}
	e.accepted = append(e.accepted, c)
	close(e.acceptNotify)
	e.acceptNotify = make(chan struct{})
}

func (e *Endpoint) wakeAcceptClosed() {
	e.acceptMu.Lock()
	defer e.acceptMu.Unlock()
	if e.acceptNotify == nil {
		e.acceptClosed = true
		return
	}
	e.acceptClosed = true
	close(e.acceptNotify)
	e.acceptNotify = make(chan struct{})
}

func (e *Endpoint) hangupAll() {
	if e.dialogs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, c := range e.dialogs.calls() {
		_ = c.Hangup(ctx)
	}
}

func (e *Endpoint) setBeforeFinal(fn func(*dialog, *sip.Request)) {
	e.mu.Lock()
	e.beforeFinal = fn
	e.mu.Unlock()
}

func (e *Endpoint) runBeforeFinal(d *dialog, req *sip.Request) {
	e.mu.Lock()
	fn := e.beforeFinal
	e.mu.Unlock()
	if fn != nil {
		fn(d, req)
	}
}

func (e *Endpoint) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if e.isClosed() {
		tx.Terminate()
		return
	}
	if !inviteAllowed(req.Source(), e.cfg.AllowFrom) {
		slog.Info("dropped INVITE", "source", req.Source(), "call_id", callIDOf(req))
		tx.Terminate()
		return
	}
	if to := req.To(); to != nil {
		if tag, ok := to.Params.Get("tag"); ok && tag != "" {
			e.onReinvite(req, tx)
			return
		}
	}
	e.onNewInvite(req, tx)
}

func (e *Endpoint) onNewInvite(req *sip.Request, tx sip.ServerTransaction) {
	victim, code := e.screenReplaces(req)
	if code != 0 {
		_ = e.respond(tx, req, code, phrase(code), nil)
		return
	}
	sess, err := e.dialogUA().ReadInvite(req, tx)
	if err != nil {
		_ = e.respond(tx, req, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	d, err := newDialog(e, sess)
	if err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	var cancelled atomic.Bool
	tx.OnCancel(func(*sip.Request) { cancelled.Store(true) })
	d.tx = tx
	d.noteInitial(req)
	d.noteArrived(req)
	e.dialogs.put(d)
	var own bool
	var gen uint64
	defer func() {
		if own {
			d.endInbound(offerInInvite, gen)
		}
		d.mu.Lock()
		published := d.call
		d.mu.Unlock()
		if published == nil {
			d.stopProvisional()
			e.dialogs.drop(d.id)
			if d.pair != nil {
				_ = d.pair.Close()
				d.pair = nil
			}
		}
	}()
	code, _, reserved, gen := d.beginInbound(sip.INVITE, len(bytes.TrimSpace(req.Body())) > 0)
	if !reserved {
		status := code
		if status == 0 {
			status = sip.StatusInternalServerError
		}
		_ = e.respond(tx, req, status, phrase(status), nil)
		return
	}
	own = true
	if err := e.sessionRespond(d, sess, sip.StatusTrying, phrase(sip.StatusTrying), nil); err != nil {
		return
	}
	if cancelled.Load() || d.ended {
		return
	}
	if e.cfg.RequireAuth {
		ok, code, hdr, aerr := e.checkAuth(callIDOf(req), req)
		if aerr != nil {
			_ = e.sessionRespond(d, sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
			return
		}
		if !ok {
			var extra []sip.Header
			if hdr != nil {
				extra = append(extra, hdr)
			}
			_ = e.sessionRespond(d, sess, code, phrase(code), nil, extra...)
			return
		}
	}
	if cancelled.Load() || d.ended {
		return
	}
	desc, has, err := parseSDP(req)
	if err != nil || !has {
		_ = e.sessionRespond(d, sess, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	if _, err := chooseAudio(desc, e.cfg); err != nil {
		_ = e.sessionRespond(d, sess, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
		return
	}
	if e.root == nil {
		_ = e.sessionRespond(d, sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	pair, err := e.AllocateRTP(e.root)
	if err != nil {
		_ = e.sessionRespond(d, sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	d.pair = pair
	if cancelled.Load() || d.ended {
		return
	}
	_, raw, err := d.origin.Answer(e.mediaAddr(pair), desc, e.cfg)
	if err != nil {
		code := sip.StatusInternalServerError
		if errors.Is(err, ErrNoCommonCodec) {
			code = sip.StatusNotAcceptableHere
		}
		_ = e.sessionRespond(d, sess, code, phrase(code), nil)
		return
	}
	d.storeSDP(raw)
	e.runBeforeFinal(d, req)
	if cancelled.Load() || d.isEnded() {
		return
	}
	if !d.claimFinal() {
		return
	}
	res := sip.NewResponseFromRequest(sess.InviteRequest, sip.StatusOK, phrase(sip.StatusOK), raw)
	e.decorate(res, true)
	d.stamp(res, nil)
	if h := e.noteInboundSession(d, req); h != nil {
		res.RemoveHeader("Supported")
		res.AppendHeader(timerSupported())
		res.AppendHeader(h)
	}
	if err := tx.Respond(res); err != nil {
		return
	}
	sess.InviteResponse = res
	d.capture(res)
	d.endInbound(offerInInvite, gen)
	own = false
	d.setAckPending(true)
	d.stopProvisional()
	ack, err := waitACK(tx, res, d)
	d.setAckPending(false)
	if err != nil || ack == nil {
		return
	}
	if err := d.noteRemote(ack); err != nil {
		return
	}
	if err := sess.ReadAck(ack, tx); err != nil {
		return
	}
	if d.ended {
		return
	}
	if err := d.establish(desc); err != nil {
		return
	}
	call := &Call{ep: e, dlg: d, done: make(chan struct{})}
	d.mu.Lock()
	d.call = call
	d.mu.Unlock()
	if e.cfg.MaxCall > 0 {
		call.max = time.AfterFunc(e.cfg.MaxCall, func() {
			_ = call.Hangup(context.Background())
		})
	}
	d.armStoredSession()
	if victim != nil {
		e.completeReplace(victim, call)
	}
	e.deliver(call)
}

func (d *dialog) establish(offer Description) error {
	remote, dir, err := mediaFromOffer(offer, d.ep.cfg)
	if err != nil {
		return err
	}
	ctx := d.ep.root
	if ctx == nil {
		ctx = context.Background()
	}
	media, err := StartMedia(ctx, d.pair, remote)
	if err != nil {
		return err
	}
	if err := applyMediaDirection(media, dir); err != nil {
		_ = media.Close()
		return err
	}
	d.mu.Lock()
	d.media = media
	d.remote = remote
	d.haveRemoteMedia = true
	d.direction = dir
	d.mu.Unlock()
	return nil
}

func (e *Endpoint) onReinvite(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d != nil {
		d.noteArrived(req)
	}
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if err := d.noteRemote(req); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	if d.sess != nil {
		_ = d.sess.ReadRequest(req, tx)
	}
	desc, has, err := parseSDP(req)
	if err != nil {
		_ = e.respond(tx, req, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	code, retry, reserved, gen := d.beginInbound(sip.INVITE, has)
	if !reserved {
		e.glareRespond(tx, req, code, retry)
		return
	}
	own := true
	defer func() {
		if own {
			d.endInbound(offerInInvite, gen)
		}
	}()
	d.mu.Lock()
	d.tx = tx
	d.mu.Unlock()
	d.storeContact(req)
	e.runBeforeFinal(d, req)
	body := d.currentSDP()
	var remote RemoteMedia
	var dir Direction
	commit := false
	if has {
		if _, aerr := chooseAudio(desc, e.cfg); aerr != nil {
			_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
			return
		}
		_, raw, aerr := d.origin.Answer(e.mediaAddr(d.pair), desc, e.cfg)
		if aerr != nil {
			_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
			return
		}
		got, gdir, merr := mediaFromOffer(desc, e.cfg)
		if merr != nil {
			_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
			return
		}
		body = raw
		remote = got
		dir = gdir
		commit = true
	}
	res := sip.NewResponseFromRequest(req, sip.StatusOK, phrase(sip.StatusOK), body)
	e.decorate(res, len(body) > 0)
	d.stamp(res, nil)
	d.setAckPending(true)
	if commit {
		err = d.commitIfSent(remote, dir, body, func() error { return tx.Respond(res) })
	} else {
		err = tx.Respond(res)
	}
	if err != nil {
		d.setAckPending(false)
		return
	}
	d.rearmSession()
	d.endInbound(offerInInvite, gen)
	own = false
	defer d.setAckPending(false)
	ack, err := waitACK(tx, res, d)
	if err != nil || ack == nil {
		return
	}
	if err := d.noteRemote(ack); err != nil {
		return
	}
	if d.sess != nil {
		_ = d.sess.ReadAck(ack, tx)
	}
}

func (e *Endpoint) onUpdate(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d != nil {
		d.noteArrived(req)
	}
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if err := d.noteRemote(req); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	if d.sess != nil {
		_ = d.sess.ReadRequest(req, tx)
	}
	desc, has, err := parseSDP(req)
	if err != nil {
		_ = e.respond(tx, req, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	code, retry, reserved, gen := d.beginInbound(sip.UPDATE, has)
	if !reserved {
		if code != 0 {
			e.glareRespond(tx, req, code, retry)
			return
		}
		d.storeContact(req)
		_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
		d.rearmSession()
		return
	}
	own := true
	defer func() {
		if own {
			d.endInbound(offerInUpdate, gen)
		}
	}()
	d.storeContact(req)
	if _, aerr := chooseAudio(desc, e.cfg); aerr != nil {
		_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
		return
	}
	_, raw, aerr := d.origin.Answer(e.mediaAddr(d.pair), desc, e.cfg)
	if aerr != nil || d.pair == nil {
		_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
		return
	}
	remote, dir, merr := mediaFromOffer(desc, e.cfg)
	if merr != nil {
		_ = e.respond(tx, req, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
		return
	}
	res := sip.NewResponseFromRequest(req, sip.StatusOK, phrase(sip.StatusOK), raw)
	e.decorate(res, true)
	d.stamp(res, nil)
	if err := d.commitIfSent(remote, dir, raw, func() error { return tx.Respond(res) }); err != nil {
		return
	}
	d.rearmSession()
	d.endInbound(offerInUpdate, gen)
	own = false
}

func (e *Endpoint) glareRespond(tx sip.ServerTransaction, req *sip.Request, code, retry int) {
	var extra []sip.Header
	if code == sip.StatusInternalServerError {
		if retry < 0 {
			retry = 0
		}
		extra = append(extra, sip.NewHeader("Retry-After", strconv.Itoa(retry)))
	}
	_ = e.respond(tx, req, code, phrase(code), nil, extra...)
}

func (e *Endpoint) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	if e.isClosed() {
		tx.Terminate()
		return
	}
	if !inviteAllowed(req.Source(), e.cfg.AllowFrom) {
		slog.Info("dropped OPTIONS", "source", req.Source(), "call_id", callIDOf(req))
		tx.Terminate()
		return
	}
	if d := e.findDialog(req); d != nil {
		d.noteArrived(req)
	} else {
		e.noteArrived(req)
	}
	res := sip.NewResponseFromRequest(req, sip.StatusOK, phrase(sip.StatusOK), nil)
	e.decorate(res, false)
	if e.cfg.SessionTimer > 0 {
		res.RemoveHeader("Supported")
		res.AppendHeader(timerSupported())
	}
	res.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
	if d := e.findDialog(req); d != nil {
		d.stamp(res, nil)
	} else {
		e.stamp(res)
	}
	_ = tx.Respond(res)
}

func (e *Endpoint) onAck(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d == nil {
		return
	}
	select {
	case d.extraAck <- req:
	default:
	}
	_ = tx
}

func (e *Endpoint) onBye(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d != nil {
		d.noteArrived(req)
	}
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	d.mu.Lock()
	held := d.holdFinish || d.notes != nil
	base := uint32(0)
	if d.haveRemote {
		base = d.remoteCSeq
	}
	d.mu.Unlock()
	if err := d.noteRemote(req); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	if held {
		if cseq := req.CSeq(); cseq != nil && cseq.SeqNo > base {
			d.mu.Lock()
			d.referByeBase = base
			d.referByeSeq = cseq.SeqNo
			d.mu.Unlock()
		}
	}
	_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
	d.mu.Lock()
	d.byeSent = true
	call := d.call
	held = d.holdFinish || d.notes != nil
	d.mu.Unlock()
	d.markEnded()
	if call != nil {
		if !held {
			call.finish()
		}
		return
	}
	e.dialogs.drop(d.id)
	if d.pair != nil {
		_ = d.pair.Close()
		d.pair = nil
	}
}

func (e *Endpoint) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
}

func ackMatches(ack *sip.Request, res *sip.Response) bool {
	if ack == nil || res == nil {
		return false
	}
	ac := ack.CSeq()
	rc := res.CSeq()
	if ac == nil || rc == nil {
		return false
	}
	return ac.MethodName == sip.ACK && ac.SeqNo == rc.SeqNo
}

func waitACK(tx sip.ServerTransaction, res *sip.Response, d *dialog) (*sip.Request, error) {
	timer := time.NewTimer(sip.T1)
	defer timer.Stop()
	end := time.NewTimer(64 * sip.T1)
	defer end.Stop()
	gap := sip.T1
	for {
		select {
		case ack := <-tx.Acks():
			if ackMatches(ack, res) {
				return ack, nil
			}
		case ack := <-d.extraAck:
			if ackMatches(ack, res) {
				return ack, nil
			}
		case <-timer.C:
			if err := tx.Respond(res); err != nil {
				return nil, err
			}
			gap *= 2
			if gap > sip.T2 {
				gap = sip.T2
			}
			timer.Reset(gap)
		case <-end.C:
			return nil, errors.New("ack timed out")
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("invite transaction ended")
		}
	}
}

func parseSDP(req *sip.Request) (Description, bool, error) {
	body := bytes.TrimSpace(req.Body())
	if len(body) == 0 {
		return Description{}, false, nil
	}
	desc, err := parseSDPText(string(body))
	if err != nil {
		return Description{}, true, err
	}
	return desc, true, nil
}

func parseSDPText(raw string) (Description, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Description{}, errors.New("sdp is empty")
	}
	// pion's SDP reader returns EOF when the last line has no newline.
	if !strings.HasSuffix(text, "\n") {
		text += "\r\n"
	}
	return Parse(text)
}

func (e *Endpoint) findDialog(req *sip.Request) *dialog {
	if d := e.dialogs.get(dialogID(req)); d != nil {
		return d
	}
	id, err := sip.DialogIDFromRequestUAC(req)
	if err != nil {
		return nil
	}
	return e.dialogs.get(id)
}

func dialogID(req *sip.Request) string {
	id, err := sip.DialogIDFromRequestUAS(req)
	if err != nil {
		return ""
	}
	return id
}
