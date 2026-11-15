package sip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// sipStatusError is a final SIP response that failed a dial or a transfer.
type sipStatusError struct {
	code int
}

func (e *sipStatusError) Error() string {
	if e == nil {
		return "sip status"
	}
	return fmt.Sprintf("sip status %d %s", e.code, phrase(e.code))
}

// Dial places one INVITE. A 401 or 407 is retried once. A second challenge
// fails the dial. A 183 with SDP plays early media. The Call is returned
// after ACK on 200. ACK and CANCEL use the INVITE CSeq.
func (e *Endpoint) Dial(ctx context.Context, uri string) (*Call, error) {
	return e.dial(ctx, uri, "", nil)
}

func (e *Endpoint) dial(ctx context.Context, rawURI, replaces string, progress func(code int, reason string)) (*Call, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.isClosed() {
		return nil, ErrEndpointClosed
	}
	target, err := parseTarget(rawURI)
	if err != nil {
		return nil, err
	}
	pair, err := e.AllocateRTP(ctx)
	if err != nil {
		return nil, err
	}
	origin, err := NewOrigin()
	if err != nil {
		_ = pair.Close()
		return nil, err
	}
	d := &dialog{
		ep:         e,
		uac:        true,
		ownsCallID: true,
		origin:     origin,
		pair:       pair,
		wake:       make(chan struct{}),
		extraAck:   make(chan *sip.Request, 1),
		prackSeen:  map[uint32]struct{}{},
	}
	var call *Call
	defer func() {
		if call != nil {
			return
		}
		d.stopInvite()
		if d.id != "" {
			e.dialogs.drop(d.id)
		}
		_ = pair.Close()
	}()

	offered, raw, err := d.origin.Offer(e.mediaAddr(pair), e.cfg, DirectionSendRecv)
	if err != nil {
		return nil, err
	}
	d.storeSDP(raw)
	d.timerOffer = sessionSeconds(e.cfg.SessionTimer)
	seq, err := d.nextCSeq(sip.INVITE)
	if err != nil {
		return nil, err
	}
	req, err := e.inviteRequest(target, raw, seq, replaces)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.invite = req
	d.offer = offerOutInvite
	d.outOffer = true
	d.mu.Unlock()
	defer d.endOutbound(offerOutInvite)

	sess := &sipgo.DialogClientSession{
		Dialog: sipgo.Dialog{InviteRequest: req},
		UA:     e.dialogUA(),
	}
	sess.Init()
	d.client = sess

	tx, err := e.beginTx(ctx, req)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.inviteTx = tx
	d.mu.Unlock()

	call, err = d.waitInvite(ctx, tx, req, offered, progress)
	if err != nil {
		return nil, err
	}
	return call, nil
}

func (e *Endpoint) inviteRequest(target sip.Uri, body []byte, seq uint32, replaces string) (*sip.Request, error) {
	req := sip.NewRequest(sip.INVITE, target)
	req.SetTransport("UDP")
	req.AppendHeader(e.via())
	from := e.localFrom()
	req.AppendHeader(from)
	to := &sip.ToHeader{Address: sip.Uri{Scheme: target.Scheme, User: target.User, Host: target.Host}}
	req.AppendHeader(to)
	callID := sip.CallIDHeader(newCallID())
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.INVITE})
	max := sip.MaxForwardsHeader(70)
	req.AppendHeader(&max)
	req.AppendHeader(sip.HeaderClone(&e.contact))
	req.AppendHeader(e.allowHeader())
	if secs := sessionSeconds(e.cfg.SessionTimer); secs > 0 {
		req.AppendHeader(timerSupported())
		req.AppendHeader(sip.NewHeader("Session-Expires", fmt.Sprintf("%d;refresher=uac", secs)))
	} else {
		req.AppendHeader(e.supportedHeader())
	}
	if replaces != "" {
		req.AppendHeader(sip.NewHeader("Replaces", replaces))
	}
	ct := sip.ContentTypeHeader("application/sdp")
	req.AppendHeader(&ct)
	req.SetBody(body)
	dest := target.HostPort()
	if target.Port == 0 {
		dest = net.JoinHostPort(target.Host, "5060")
	}
	req.SetDestination(dest)
	req.Laddr = e.listen
	return req, nil
}

func (e *Endpoint) localFrom() *sip.FromHeader {
	from := &sip.FromHeader{
		DisplayName: e.cfg.DisplayName,
		Address: sip.Uri{
			Scheme: "sip",
			User:   e.cfg.User,
			Host:   e.cfg.Domain,
		},
		Params: sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	return from
}

func (d *dialog) waitInvite(ctx context.Context, tx sip.ClientTransaction, invite *sip.Request, offered Description, progress func(int, string)) (*Call, error) {
	authTries := 0
	seTries := 0
	saw1xx := false
	resend := func() (sip.ClientTransaction, error) {
		seq, err := d.nextCSeq(sip.INVITE)
		if err != nil {
			return nil, err
		}
		if cseq := invite.CSeq(); cseq != nil {
			cseq.SeqNo = seq
			cseq.MethodName = sip.INVITE
		}
		invite.RemoveHeader("Via")
		tx.Terminate()
		next, err := d.ep.beginTx(ctx, invite, sipgo.ClientRequestAddVia)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.invite = invite
		d.inviteTx = next
		d.mu.Unlock()
		return next, nil
	}
	for {
		var res *sip.Response
		ended := false
		select {
		case got, ok := <-tx.Responses():
			if !ok {
				return d.failBeforeAnswer(invite, errors.New("transaction ended"))
			}
			res = got
		case <-tx.Done():
			ended = true
			select {
			case got, ok := <-tx.Responses():
				if ok {
					res = got
				}
			default:
			}
		case <-ctx.Done():
			final := d.abortInvite(tx, invite, saw1xx)
			if final != nil && final.StatusCode == sip.StatusOK {
				_ = d.ackOutbound(invite, final)
				_ = d.sendBye(context.Background())
			}
			return nil, ctx.Err()
		}
		if res == nil {
			if ended {
				err := tx.Err()
				if err == nil {
					err = errors.New("transaction ended")
				}
				return d.failBeforeAnswer(invite, err)
			}
			continue
		}
		d.learnResponse(res, invite)
		if res.IsProvisional() {
			saw1xx = true
			var earlyErr error
			if res.StatusCode == sip.StatusSessionInProgress {
				earlyErr = d.playEarly(res, offered)
			}
			if progress != nil && earlyErr == nil && res.StatusCode > sip.StatusTrying {
				progress(res.StatusCode, reasonText(res))
			}
			_ = d.prack(ctx, res, invite)
			if earlyErr != nil {
				return d.failBeforeAnswer(invite, earlyErr)
			}
			continue
		}
		if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
			if authTries >= 1 || eAuthMissing(d.ep) {
				return nil, &sipStatusError{code: res.StatusCode}
			}
			authTries++
			next, err := d.retryAuth(ctx, tx, invite, res)
			if err != nil {
				return nil, err
			}
			tx = next
			continue
		}
		if res.StatusCode == 422 && seTries < 1 && d.timerOffer > 0 {
			if minSE := parseMinSE(res); minSE > d.timerOffer {
				seTries++
				d.timerOffer = minSE
				invite.RemoveHeader("Session-Expires")
				invite.AppendHeader(sip.NewHeader("Session-Expires", fmt.Sprintf("%d;refresher=uac", minSE)))
				next, err := resend()
				if err != nil {
					return nil, err
				}
				tx = next
				continue
			}
		}
		if res.StatusCode != sip.StatusOK {
			return nil, &sipStatusError{code: res.StatusCode}
		}
		if err := d.ackOutbound(invite, res); err != nil {
			return nil, err
		}
		if err := d.confirm(res, offered); err != nil {
			_ = d.sendBye(context.Background())
			return nil, err
		}
		d.adoptOutboundSession(res)
		return d.publish(), nil
	}
}

// failBeforeAnswer cancels an INVITE that has not reached 200. A final
// response is not this path: that response is already acknowledged.
func (d *dialog) failBeforeAnswer(invite *sip.Request, err error) (*Call, error) {
	_ = d.sendCancel(invite)
	if err == nil {
		err = errors.New("transaction ended")
	}
	return nil, err
}

func eAuthMissing(e *Endpoint) bool {
	return e == nil || e.cfg.Auth == nil || e.authPass() == ""
}

func (d *dialog) retryAuth(ctx context.Context, tx sip.ClientTransaction, invite *sip.Request, res *sip.Response) (sip.ClientTransaction, error) {
	if err := applyChallenge(invite, res, d.ep.authUser(), d.ep.authPass()); err != nil {
		return nil, err
	}
	seq, err := d.nextCSeq(sip.INVITE)
	if err != nil {
		return nil, err
	}
	if cseq := invite.CSeq(); cseq != nil {
		cseq.SeqNo = seq
		cseq.MethodName = sip.INVITE
	}
	invite.RemoveHeader("Via")
	tx.Terminate()
	next, err := d.ep.beginTx(ctx, invite, sipgo.ClientRequestAddVia)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.invite = invite
	d.inviteTx = next
	d.mu.Unlock()
	return next, nil
}

func applyChallenge(req *sip.Request, res *sip.Response, user, pass string) error {
	proxy := res.StatusCode == sip.StatusProxyAuthRequired
	chalName := "WWW-Authenticate"
	credName := "Authorization"
	if proxy {
		chalName = "Proxy-Authenticate"
		credName = "Proxy-Authorization"
	}
	h := res.GetHeader(chalName)
	if h == nil {
		return errors.New("challenge is missing")
	}
	chal, err := digest.ParseChallenge(h.Value())
	if err != nil {
		return err
	}
	if chal.Algorithm == "" {
		chal.Algorithm = "MD5"
	}
	chal.Algorithm = strings.ToUpper(chal.Algorithm)
	cred, err := digest.Digest(chal, digest.Options{
		Method:   string(sip.INVITE),
		URI:      req.Recipient.Addr(),
		Username: user,
		Password: pass,
	})
	if err != nil {
		return err
	}
	req.RemoveHeader(credName)
	req.AppendHeader(sip.NewHeader(credName, cred.String()))
	return nil
}

func (d *dialog) playEarly(res *sip.Response, offered Description) error {
	body := bytesTrim(res.Body())
	if len(body) == 0 {
		return nil
	}
	ctx := d.ep.root
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	current := d.media
	pair := d.pair
	d.mu.Unlock()
	media, err := ApplyEarly(ctx, pair, current, offered, body)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.media = media
	d.mu.Unlock()
	return nil
}

func (d *dialog) confirm(res *sip.Response, offered Description) error {
	body := bytesTrim(res.Body())
	if len(body) == 0 {
		return errors.New("sdp answer is missing")
	}
	ctx := d.ep.root
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	current := d.media
	pair := d.pair
	d.mu.Unlock()
	media, err := ConfirmAnswer(ctx, pair, current, offered, body)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.media = media
	d.finalSent = true
	d.mu.Unlock()
	return nil
}

func (d *dialog) publish() *Call {
	call := &Call{ep: d.ep, dlg: d, done: make(chan struct{})}
	d.mu.Lock()
	d.call = call
	d.finalSent = true
	d.mu.Unlock()
	if d.ep.cfg.MaxCall > 0 {
		call.max = time.AfterFunc(d.ep.cfg.MaxCall, func() {
			_ = call.Hangup(context.Background())
		})
	}
	d.armStoredSession()
	return call
}

func (d *dialog) learnResponse(res *sip.Response, invite *sip.Request) {
	if res == nil || invite == nil {
		return
	}
	d.mu.Lock()
	if d.localFrom == nil && invite.From() != nil {
		d.localFrom = sip.HeaderClone(invite.From())
	}
	if to := res.To(); to != nil {
		d.remoteTo = sip.HeaderClone(to)
		if tag, ok := to.Params.Get("tag"); ok && tag != "" {
			d.party.RemoteTag = tag
		}
		if to.DisplayName != "" {
			d.party.DisplayName = to.DisplayName
		}
	}
	if from := invite.From(); from != nil {
		if tag, ok := from.Params.Get("tag"); ok {
			d.party.LocalTag = tag
		}
	}
	if d.callID == nil && invite.CallID() != nil {
		d.callID = sip.HeaderClone(invite.CallID())
	}
	if cid := invite.CallID(); cid != nil {
		d.party.CallID = cid.Value()
	}
	if d.party.URI == "" {
		d.party.URI = invite.Recipient.String()
	}
	if c := res.Contact(); c != nil {
		d.remoteURI = *c.Address.Clone()
		d.haveURI = true
		d.party.RemoteTarget = c.Address.String()
		d.party.URI = c.Address.String()
	}
	if hasToken(res, "Allow", "UPDATE") {
		d.allowUpdate = true
	}
	var prev, id string
	if d.party.CallID != "" && d.party.LocalTag != "" && d.party.RemoteTag != "" {
		id = sip.DialogIDMake(d.party.CallID, d.party.RemoteTag, d.party.LocalTag)
		if d.id != id {
			prev = d.id
			d.id = id
		}
	}
	d.mu.Unlock()
	if prev != "" {
		d.ep.dialogs.drop(prev)
	}
	if id != "" {
		d.ep.dialogs.put(d)
	}
}

func (d *dialog) prack(ctx context.Context, res *sip.Response, invite *sip.Request) error {
	rseq, ok := responseRSeq(res)
	if !ok {
		return nil
	}
	d.mu.Lock()
	if _, seen := d.prackSeen[rseq]; seen {
		d.mu.Unlock()
		return nil
	}
	inviteSeq := uint32(0)
	if cseq := invite.CSeq(); cseq != nil {
		inviteSeq = cseq.SeqNo
	}
	d.mu.Unlock()
	rack := fmt.Sprintf("%d %d INVITE", rseq, inviteSeq)
	req, err := d.craft(sip.PRACK, nil, "", []sip.Header{sip.NewHeader("RAck", rack)})
	if err != nil {
		return err
	}
	got, err := d.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if got.StatusCode < 200 || got.StatusCode >= 300 {
		return &sipStatusError{code: got.StatusCode}
	}
	d.mu.Lock()
	if d.prackSeen == nil {
		d.prackSeen = map[uint32]struct{}{}
	}
	d.prackSeen[rseq] = struct{}{}
	d.mu.Unlock()
	return nil
}

func responseRSeq(res *sip.Response) (uint32, bool) {
	if res == nil || res.StatusCode <= sip.StatusTrying || res.StatusCode >= 200 {
		return 0, false
	}
	h := res.GetHeader("RSeq")
	if h == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(h.Value()), 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

func (d *dialog) ackOutbound(invite *sip.Request, res *sip.Response) error {
	if err := d.ackInvite(invite, res); err != nil {
		return err
	}
	return nil
}

func (d *dialog) abortInvite(tx sip.ClientTransaction, invite *sip.Request, saw1xx bool) *sip.Response {
	if !saw1xx {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case res := <-tx.Responses():
			if res != nil && !res.IsProvisional() {
				return res
			}
			if res != nil {
				d.learnResponse(res, invite)
			}
		case <-tx.Done():
			return nil
		case <-timer.C:
		}
	}
	_ = d.sendCancel(invite)
	return awaitFinal(tx, time.Second)
}

func (d *dialog) sendCancel(invite *sip.Request) error {
	if invite == nil || d.ep == nil || d.ep.sipCli == nil {
		return errors.New("cancel is missing the invite")
	}
	cancel := cancelFrom(invite)
	tx, err := d.ep.beginTx(context.Background(), cancel)
	if err != nil {
		return err
	}
	defer tx.Terminate()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-tx.Responses():
		return nil
	case <-tx.Done():
		return tx.Err()
	case <-timer.C:
		return nil
	}
}

func cancelFrom(invite *sip.Request) *sip.Request {
	cancel := sip.NewRequest(sip.CANCEL, invite.Recipient)
	cancel.SetTransport(invite.Transport())
	if via := invite.Via(); via != nil {
		cancel.AppendHeader(via.Clone())
	}
	if from := invite.From(); from != nil {
		cancel.AppendHeader(sip.HeaderClone(from))
	}
	if to := invite.To(); to != nil {
		cancel.AppendHeader(sip.HeaderClone(to))
	}
	if cid := invite.CallID(); cid != nil {
		cancel.AppendHeader(sip.HeaderClone(cid))
	}
	seq := uint32(0)
	if cseq := invite.CSeq(); cseq != nil {
		seq = cseq.SeqNo
	}
	cancel.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.CANCEL})
	max := sip.MaxForwardsHeader(70)
	cancel.AppendHeader(&max)
	cancel.SetBody(nil)
	cancel.SetDestination(invite.Destination())
	cancel.Laddr = invite.Laddr
	return cancel
}

func awaitFinal(tx sip.ClientTransaction, d time.Duration) *sip.Response {
	if tx == nil {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				return nil
			}
			if res == nil || res.IsProvisional() {
				continue
			}
			return res
		case <-tx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	}
}

// beginTx sends a request. Start and the UDP reader share one socket, and the
// reader registers that socket asynchronously. Until it does, a client bind of
// the same address fails. Retry that race, not a failure to reach the peer.
func (e *Endpoint) beginTx(ctx context.Context, req *sip.Request, opts ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	var tx sip.ClientTransaction
	var err error
	for i := 0; i < 100; i++ {
		tx, err = e.sipCli.TransactionRequest(ctx, req, opts...)
		if err == nil || !bindBusy(err) {
			return tx, err
		}
		timer := time.NewTimer(2 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, err
}

func (e *Endpoint) writeReq(req *sip.Request) error {
	var err error
	for i := 0; i < 100; i++ {
		err = e.sipCli.WriteRequest(req)
		if err == nil || !bindBusy(err) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

func bindBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

func (d *dialog) stopInvite() {
	d.mu.Lock()
	tx := d.inviteTx
	d.inviteTx = nil
	d.mu.Unlock()
	if tx != nil {
		tx.Terminate()
	}
}

func (d *dialog) isEnded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ended
}

func (d *dialog) claimFinal() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended || d.finalSent {
		return false
	}
	d.finalSent = true
	return true
}

func parseTarget(raw string) (sip.Uri, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")
	}
	var u sip.Uri
	if err := sip.ParseUri(s, &u); err != nil {
		return sip.Uri{}, err
	}
	if u.Scheme == "" {
		u.Scheme = "sip"
	}
	if !strings.EqualFold(u.Scheme, "sip") && !strings.EqualFold(u.Scheme, "sips") {
		return sip.Uri{}, errors.New("unsupported uri scheme")
	}
	if u.Host == "" {
		return sip.Uri{}, errors.New("uri host is missing")
	}
	return u, nil
}

func newCallID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func reasonText(res *sip.Response) string {
	if res == nil {
		return ""
	}
	if res.Reason != "" {
		return res.Reason
	}
	return phrase(res.StatusCode)
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func statusOf(err error) (int, string) {
	var se *sipStatusError
	if errors.As(err, &se) && se != nil {
		return se.code, phrase(se.code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return sip.StatusRequestTimeout, phrase(sip.StatusRequestTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return sip.StatusRequestTerminated, phrase(sip.StatusRequestTerminated)
	}
	return sip.StatusServiceUnavailable, phrase(sip.StatusServiceUnavailable)
}
