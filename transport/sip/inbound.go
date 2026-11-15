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

func (e *Endpoint) openSIP(advIP net.IP, advPort int, udp *net.UDPConn, tcp *net.TCPListener) error {
	if udp == nil {
		return nil
	}
	host, port, err := sip.ParseAddr(udp.LocalAddr().String())
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("sip listen address is missing")
	}
	e.listen = sip.Addr{IP: append(net.IP(nil), ip...), Port: port, Hostname: ip.String()}
	params := sip.NewParams()
	params.Add("transport", "udp")
	e.contact = sip.ContactHeader{
		Address: sip.Uri{
			Scheme:    "sip",
			User:      e.cfg.User,
			Host:      advIP.String(),
			Port:      advPort,
			UriParams: params,
		},
	}
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("jargo-sip"),
		sipgo.WithUserAgentHostname(advIP.String()),
	)
	if err != nil {
		return err
	}
	cli, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname(advIP.String()),
		sipgo.WithClientPort(advPort),
		sipgo.WithClientConnectionAddr(e.listen.String()),
	)
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
	e.serveWG.Add(1)
	go func() {
		defer e.serveWG.Done()
		_ = srv.ServeUDP(udp)
	}()
	if tcp != nil {
		e.serveWG.Add(1)
		go func() {
			defer e.serveWG.Done()
			_ = srv.ServeTCP(tcp)
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
	e.dialogs.put(d)
	defer func() {
		d.endInbound(offerInInvite)
		if d.call == nil {
			d.stopProvisional()
			e.dialogs.drop(d.id)
			if d.pair != nil {
				_ = d.pair.Close()
				d.pair = nil
			}
		}
	}()
	if _, _, ok := d.beginInbound(sip.INVITE, len(bytes.TrimSpace(req.Body())) > 0); !ok {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	if err := e.sessionRespond(sess, sip.StatusTrying, phrase(sip.StatusTrying), nil); err != nil {
		return
	}
	if cancelled.Load() || d.ended {
		return
	}
	if e.cfg.RequireAuth {
		ok, code, hdr, aerr := e.checkAuth(callIDOf(req), req)
		if aerr != nil {
			_ = e.sessionRespond(sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
			return
		}
		if !ok {
			var extra []sip.Header
			if hdr != nil {
				extra = append(extra, hdr)
			}
			_ = e.sessionRespond(sess, code, phrase(code), nil, extra...)
			return
		}
	}
	if cancelled.Load() || d.ended {
		return
	}
	desc, has, err := parseSDP(req)
	if err != nil || !has {
		_ = e.sessionRespond(sess, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	if _, err := chooseAudio(desc, e.cfg); err != nil {
		_ = e.sessionRespond(sess, sip.StatusNotAcceptableHere, phrase(sip.StatusNotAcceptableHere), nil)
		return
	}
	if e.root == nil {
		_ = e.sessionRespond(sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	pair, err := e.AllocateRTP(e.root)
	if err != nil {
		_ = e.sessionRespond(sess, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
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
		_ = e.sessionRespond(sess, code, phrase(code), nil)
		return
	}
	d.storeSDP(raw)
	e.runBeforeFinal(d, req)
	if cancelled.Load() || d.ended {
		return
	}
	res := sip.NewResponseFromRequest(sess.InviteRequest, sip.StatusOK, phrase(sip.StatusOK), raw)
	e.decorate(res, true)
	if err := tx.Respond(res); err != nil {
		return
	}
	sess.InviteResponse = res
	d.capture(res)
	d.endInbound(offerInInvite)
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
	d.call = call
	if e.cfg.MaxCall > 0 {
		call.max = time.AfterFunc(e.cfg.MaxCall, func() {
			_ = call.Hangup(context.Background())
		})
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
	d := e.dialogs.get(dialogID(req))
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
	code, retry, ok := d.beginInbound(sip.INVITE, has)
	if !ok {
		e.glareRespond(tx, req, code, retry)
		return
	}
	defer d.endInbound(offerInInvite)
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
	if commit {
		if err := d.commitMedia(remote, dir); err != nil {
			_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
			return
		}
		d.storeSDP(body)
	}
	res := sip.NewResponseFromRequest(req, sip.StatusOK, phrase(sip.StatusOK), body)
	e.decorate(res, len(body) > 0)
	d.setAckPending(true)
	if err := tx.Respond(res); err != nil {
		d.setAckPending(false)
		return
	}
	d.endInbound(offerInInvite)
	defer d.setAckPending(false)
	ack, err := waitACK(tx, res, d)
	if err != nil || ack == nil {
		return
	}
	_ = d.noteRemote(ack)
	if d.sess != nil {
		_ = d.sess.ReadAck(ack, tx)
	}
}

func (e *Endpoint) onUpdate(req *sip.Request, tx sip.ServerTransaction) {
	d := e.dialogs.get(dialogID(req))
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
	code, retry, ok := d.beginInbound(sip.UPDATE, has)
	if !ok {
		e.glareRespond(tx, req, code, retry)
		return
	}
	defer d.endInbound(offerInUpdate)
	d.storeContact(req)
	if !has {
		_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
		return
	}
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
	if err := d.commitMedia(remote, dir); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	d.storeSDP(raw)
	res := sip.NewResponseFromRequest(req, sip.StatusOK, phrase(sip.StatusOK), raw)
	e.decorate(res, true)
	if err := tx.Respond(res); err != nil {
		return
	}
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

func (e *Endpoint) onAck(req *sip.Request, tx sip.ServerTransaction) {
	d := e.dialogs.get(dialogID(req))
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
	d := e.dialogs.get(dialogID(req))
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if err := d.noteRemote(req); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
	d.mu.Lock()
	d.byeSent = true
	call := d.call
	d.mu.Unlock()
	d.markEnded()
	if call != nil {
		call.finish()
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

func waitACK(tx sip.ServerTransaction, res *sip.Response, d *dialog) (*sip.Request, error) {
	timer := time.NewTimer(sip.T1)
	defer timer.Stop()
	end := time.NewTimer(64 * sip.T1)
	defer end.Stop()
	gap := sip.T1
	for {
		select {
		case ack := <-tx.Acks():
			return ack, nil
		case ack := <-d.extraAck:
			return ack, nil
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

func dialogID(req *sip.Request) string {
	id, err := sip.DialogIDFromRequestUAS(req)
	if err != nil {
		return ""
	}
	return id
}
