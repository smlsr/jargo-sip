package sip

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// referNote is one NOTIFY sipfrag on a REFER subscription.
type referNote struct {
	code       int
	terminated bool
}

var errBadRefer = errors.New("refer-to is invalid")

// OnReplaced stores the callback for a dialog replaced by REFER or Replaces.
// A later call replaces it. Nil clears it. The callback runs before the old
// call's Done channel closes.
func (e *Endpoint) OnReplaced(fn func(old, new *Call)) {
	e.mu.Lock()
	e.onReplaced = fn
	e.mu.Unlock()
}

func (e *Endpoint) fireReplaced(old, new *Call) {
	e.mu.Lock()
	fn := e.onReplaced
	e.mu.Unlock()
	if fn != nil {
		fn(old, new)
	}
}

// Transfer sends a blind REFER. NOTIFY 200 ends this call. A failure
// sipfrag, or no NOTIFY before the subscription expires, leaves it up.
func (c *Call) Transfer(ctx context.Context, target string) error {
	return c.refer(ctx, target, "")
}

// TransferAttended sends a REFER whose Refer-To is other's remote target
// with an embedded Replaces header. other must be established.
func (c *Call) TransferAttended(ctx context.Context, other *Call) error {
	if err := other.alive(); err != nil {
		return err
	}
	party := other.Remote()
	if party.RemoteTarget == "" || party.CallID == "" || party.LocalTag == "" || party.RemoteTag == "" {
		return errors.New("attended target is not established")
	}
	replaces := fmt.Sprintf("%s;to-tag=%s;from-tag=%s", party.CallID, party.RemoteTag, party.LocalTag)
	return c.refer(ctx, party.RemoteTarget, replaces)
}

func (c *Call) alive() error {
	if c == nil || c.dlg == nil {
		return errors.New("call is missing")
	}
	select {
	case <-c.done:
		return errors.New("call has ended")
	default:
		return nil
	}
}

func (c *Call) refer(ctx context.Context, target, replaces string) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := c.alive(); err != nil {
		return err
	}
	if _, err := parseTarget(target); err != nil {
		return err
	}
	d := c.dlg
	notes := make(chan referNote, 8)
	d.mu.Lock()
	if d.ended {
		d.mu.Unlock()
		return errors.New("call has ended")
	}
	d.notes = notes
	// Hold Done across the subscription. A BYE pipelined with NOTIFY must
	// not finish the call before the sipfrag is read.
	d.holdFinish = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.notes == notes {
			d.notes = nil
		}
		d.mu.Unlock()
	}()
	defer c.finishReferWait()

	extra := []sip.Header{sip.NewHeader("Refer-To", referToValue(target, replaces))}
	if replaces != "" {
		extra = append(extra, sip.NewHeader("Referred-By", fmt.Sprintf("<sip:%s@%s>", c.ep.cfg.User, c.ep.cfg.Domain)))
	}
	req, err := d.craft(sip.REFER, nil, "", extra)
	if err != nil {
		return err
	}
	res, err := d.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if res.StatusCode != sip.StatusAccepted {
		return &sipStatusError{code: res.StatusCode}
	}
	wait := 60 * time.Second
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case note := <-notes:
			if note.code >= 200 && note.code < 300 {
				select {
				case <-c.done:
					return nil
				default:
				}
				d.holdBye()
				byeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = d.sendBye(byeCtx)
				cancel()
				c.finish()
				return nil
			}
			if note.code >= 300 || note.terminated {
				if note.code == 0 {
					note.code = sip.StatusRequestTimeout
				}
				return &sipStatusError{code: note.code}
			}
		case <-timer.C:
			return errors.New("refer subscription expired")
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			if note, ok := pullNote(notes); ok {
				if note.code >= 200 && note.code < 300 {
					return nil
				}
				if note.code >= 300 {
					if note.code == 0 {
						note.code = sip.StatusRequestTimeout
					}
					return &sipStatusError{code: note.code}
				}
			}
			return errors.New("call has ended")
		}
	}
}

// finishReferWait drops the BYE hold. A peer BYE that arrived during the
// subscription ends the call now. A failed transfer with no BYE leaves it up.
func (c *Call) finishReferWait() {
	if c == nil || c.dlg == nil {
		return
	}
	d := c.dlg
	d.mu.Lock()
	ended := d.ended
	d.holdFinish = false
	d.mu.Unlock()
	if ended {
		c.finish()
	}
}

func pullNote(notes <-chan referNote) (referNote, bool) {
	select {
	case n := <-notes:
		return n, true
	default:
		return referNote{}, false
	}
}

func (d *dialog) holdBye() {
	d.mu.Lock()
	d.holdFinish = true
	d.mu.Unlock()
}

func (e *Endpoint) onRefer(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if err := d.noteRemote(req); err != nil {
		_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
		return
	}
	target, replaces, err := referTarget(req)
	if err != nil {
		_ = e.respond(tx, req, sip.StatusBadRequest, phrase(sip.StatusBadRequest), nil)
		return
	}
	d.mu.Lock()
	call := d.call
	ended := d.ended
	busy := d.referBusy
	if call != nil && !ended && !busy {
		d.referBusy = true
	}
	d.mu.Unlock()
	if call == nil || ended {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if busy {
		_ = e.respond(tx, req, sip.StatusRequestPending, phrase(sip.StatusRequestPending), nil)
		return
	}
	_ = e.respond(tx, req, sip.StatusAccepted, phrase(sip.StatusAccepted), nil, sip.NewHeader("Expires", "60"))
	go e.followRefer(d, call, target, replaces)
}

func (e *Endpoint) followRefer(d *dialog, old *Call, target, replaces string) {
	defer func() {
		d.mu.Lock()
		d.referBusy = false
		d.mu.Unlock()
	}()
	ctx := e.root
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = d.notifyFrag(ctx, sip.StatusTrying, phrase(sip.StatusTrying), false)
	call, err := e.dial(ctx, target, replaces, func(code int, reason string) {
		if code <= sip.StatusTrying || code >= 200 {
			return
		}
		if reason == "" {
			reason = phrase(code)
		}
		_ = d.notifyFrag(ctx, code, reason, false)
	})
	if err != nil {
		code, reason := statusOf(err)
		_ = d.notifyFrag(ctx, code, reason, true)
		return
	}
	if old != nil {
		old.dlg.holdBye()
	}
	_ = d.notifyFrag(ctx, sip.StatusOK, phrase(sip.StatusOK), true)
	if old != nil {
		byeCtx, byeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.dlg.sendBye(byeCtx)
		byeCancel()
		e.fireReplaced(old, call)
		old.finish()
	}
}

func (d *dialog) notifyFrag(ctx context.Context, code int, reason string, terminated bool) error {
	if reason == "" {
		reason = phrase(code)
	}
	body := fmt.Sprintf("SIP/2.0 %d %s\r\n", code, reason)
	state := "active;expires=60"
	if terminated {
		state = "terminated;reason=noresource"
	}
	extra := []sip.Header{
		sip.NewHeader("Event", "refer"),
		sip.NewHeader("Subscription-State", state),
	}
	req, err := d.craft(sip.NOTIFY, []byte(body), "message/sipfrag;version=2.0", extra)
	if err != nil {
		return err
	}
	res, err := d.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if res.StatusCode != sip.StatusOK {
		return &sipStatusError{code: res.StatusCode}
	}
	return nil
}

func (e *Endpoint) onNotify(req *sip.Request, tx sip.ServerTransaction) {
	d := e.findDialog(req)
	if d == nil {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	if err := d.noteRemote(req); err != nil {
		if !d.lateReferNotify(req) {
			_ = e.respond(tx, req, sip.StatusInternalServerError, phrase(sip.StatusInternalServerError), nil)
			return
		}
	}
	ev := req.GetHeader("Event")
	if ev == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ev.Value())), "refer") {
		_ = e.respond(tx, req, 489, phrase(489), nil)
		return
	}
	code, _ := parseSipfrag(req.Body())
	terminated := false
	if sub := req.GetHeader("Subscription-State"); sub != nil {
		terminated = strings.Contains(strings.ToLower(sub.Value()), "terminated")
	}
	d.holdBye()
	d.pushNote(referNote{code: code, terminated: terminated})
	_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
}

// lateReferNotify accepts one refer NOTIFY whose CSeq lost the race to a
// pipelined BYE. The BYE already moved the remote sequence forward, and
// dropping this sipfrag would fail a transfer that the peer completed.
func (d *dialog) lateReferNotify(req *sip.Request) bool {
	cseq := req.CSeq()
	if cseq == nil || cseq.MethodName != sip.NOTIFY {
		return false
	}
	ev := req.GetHeader("Event")
	if ev == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ev.Value())), "refer") {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.notes == nil || !d.holdFinish {
		return false
	}
	if _, seen := d.seenRemote[cseq.SeqNo]; seen {
		return false
	}
	if d.seenRemote == nil {
		d.seenRemote = map[uint32]struct{}{}
	}
	d.seenRemote[cseq.SeqNo] = struct{}{}
	return true
}

func (d *dialog) pushNote(n referNote) {
	d.mu.Lock()
	ch := d.notes
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- n:
	default:
	}
}

type replacesHeader struct {
	callID    string
	toTag     string
	fromTag   string
	earlyOnly bool
}

func (e *Endpoint) screenReplaces(req *sip.Request) (*dialog, int) {
	h := req.GetHeader("Replaces")
	if h == nil {
		return nil, 0
	}
	rep, err := parseReplaces(h.Value())
	if err != nil {
		return nil, sip.StatusBadRequest
	}
	found := e.dialogs.find(rep.callID, rep.toTag, rep.fromTag)
	if found == nil {
		return nil, sip.StatusCallTransactionDoesNotExists
	}
	found.mu.Lock()
	ended := found.ended
	early := found.call == nil && !found.finalSent
	found.mu.Unlock()
	if ended {
		return nil, sip.StatusCallTransactionDoesNotExists
	}
	if early && !rep.earlyOnly {
		return nil, sip.StatusRequestPending
	}
	return found, 0
}

func (e *Endpoint) completeReplace(victim *dialog, newCall *Call) {
	if victim == nil {
		return
	}
	victim.mu.Lock()
	old := victim.call
	ended := victim.ended
	early := old == nil && !victim.finalSent
	uac := victim.uac
	invite := victim.invite
	victim.mu.Unlock()
	if ended {
		return
	}
	if early {
		if uac {
			victim.mu.Lock()
			victim.ended = true
			victim.mu.Unlock()
			_ = victim.sendCancel(invite)
		} else {
			_ = victim.rejectEarly(sip.StatusRequestTerminated)
		}
		e.fireReplaced(old, newCall)
		return
	}
	if old == nil {
		victim.mu.Lock()
		victim.ended = true
		victim.mu.Unlock()
		byeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = victim.sendBye(byeCtx)
		cancel()
		e.fireReplaced(nil, newCall)
		return
	}
	old.dlg.holdBye()
	byeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = old.dlg.sendBye(byeCtx)
	cancel()
	e.fireReplaced(old, newCall)
	old.finish()
}

func (d *dialog) rejectEarly(code int) bool {
	d.mu.Lock()
	if d.ended || d.finalSent || d.call != nil {
		d.mu.Unlock()
		return false
	}
	d.ended = true
	tx := d.tx
	var req *sip.Request
	if d.sess != nil {
		req = d.sess.InviteRequest
	}
	d.mu.Unlock()
	d.stopProvisional()
	if tx == nil || req == nil {
		return false
	}
	res := sip.NewResponseFromRequest(req, code, phrase(code), nil)
	d.ep.decorate(res, false)
	return tx.Respond(res) == nil
}

func referTarget(req *sip.Request) (string, string, error) {
	h := req.ReferTo()
	if h == nil || h.Address.Host == "" {
		return "", "", errBadRefer
	}
	u := h.Address
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "sip") && !strings.EqualFold(u.Scheme, "sips") {
		return "", "", errBadRefer
	}
	if m, ok := paramGet(u.UriParams, "method"); ok && !strings.EqualFold(m, "INVITE") {
		return "", "", errBadRefer
	}
	if m, ok := paramGet(u.Headers, "method"); ok && !strings.EqualFold(m, "INVITE") {
		return "", "", errBadRefer
	}
	replaces := ""
	if raw, ok := paramGet(u.Headers, "Replaces"); ok {
		raw = unescape(raw)
		if _, err := parseReplaces(raw); err != nil {
			return "", "", errBadRefer
		}
		replaces = raw
	}
	clean := u
	clean.Headers = nil
	if clean.Scheme == "" {
		clean.Scheme = "sip"
	}
	return clean.String(), replaces, nil
}

func referToValue(target, replaces string) string {
	u, err := parseTarget(target)
	if err != nil {
		return "<" + target + ">"
	}
	headers := u.Headers
	u.Headers = nil
	var b strings.Builder
	b.WriteByte('<')
	b.WriteString(u.String())
	first := true
	write := func(k, v string) {
		if first {
			b.WriteByte('?')
			first = false
		} else {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
	}
	if headers != nil {
		for _, kv := range headers {
			write(kv.K, kv.V)
		}
	}
	if replaces != "" {
		write("Replaces", replaces)
	}
	b.WriteByte('>')
	return b.String()
}

func parseReplaces(raw string) (replacesHeader, error) {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, "<>")
	v = unescape(v)
	callID, rest, _ := strings.Cut(v, ";")
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return replacesHeader{}, errBadRefer
	}
	rep := replacesHeader{callID: callID}
	if rest == "" {
		return replacesHeader{}, errBadRefer
	}
	for _, part := range strings.Split(rest, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.EqualFold(part, "early-only") {
			rep.earlyOnly = true
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		val = strings.Trim(val, "\"")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "to-tag":
			rep.toTag = val
		case "from-tag":
			rep.fromTag = val
		}
	}
	if rep.toTag == "" || rep.fromTag == "" {
		return replacesHeader{}, errBadRefer
	}
	return rep, nil
}

func parseSipfrag(body []byte) (int, string) {
	line := strings.TrimSpace(string(body))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if !strings.HasPrefix(line, "SIP/2.0") {
		return 0, ""
	}
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return 0, ""
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, ""
	}
	reason := phrase(code)
	if len(fields) == 3 && fields[2] != "" {
		reason = fields[2]
	}
	return code, reason
}

func paramGet(p sip.HeaderParams, key string) (string, bool) {
	if p == nil {
		return "", false
	}
	if v, ok := p.Get(key); ok {
		return v, true
	}
	for _, kv := range p {
		if strings.EqualFold(kv.K, key) {
			return kv.V, true
		}
	}
	return "", false
}

func unescape(s string) string {
	if out, err := url.QueryUnescape(s); err == nil {
		return out
	}
	return s
}
