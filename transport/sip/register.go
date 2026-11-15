package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// registerInitial sends the first REGISTER. A rejection or a timeout is
// returned to Start. A success starts refresh at half the agreed lifetime.
func (e *Endpoint) registerInitial(ctx context.Context) error {
	if e.root == nil {
		return errors.New("endpoint is not started")
	}
	e.regSend.Lock()
	life, err := e.sendRegister(ctx, false)
	e.regSend.Unlock()
	if err != nil {
		return err
	}
	loop, cancel := context.WithCancel(e.root)
	e.regMu.Lock()
	e.regOK = true
	e.regStop = false
	e.regCancel = cancel
	e.regMu.Unlock()
	go e.refreshRegistration(loop, life)
	return nil
}

// unregister sends Expires: 0. A failure is logged. The socket stays up
// until Close continues with its own shutdown.
func (e *Endpoint) unregister() {
	e.regMu.Lock()
	ok := e.regOK
	e.regStop = true
	cancel := e.regCancel
	e.regCancel = nil
	e.regMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !ok {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	e.regSend.Lock()
	defer e.regSend.Unlock()
	e.regMu.Lock()
	e.regOK = false
	e.regMu.Unlock()
	if e.sipCli == nil {
		return
	}
	if _, err := e.sendRegister(ctx, true); err != nil {
		slog.Error("REGISTER Expires 0 failed", "err", err)
	}
}

func (e *Endpoint) refreshRegistration(ctx context.Context, life time.Duration) {
	fails := 0
	for {
		wait := life / 2
		if wait <= 0 {
			wait = life
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		e.regSend.Lock()
		e.regMu.Lock()
		stop := e.regStop
		e.regMu.Unlock()
		if stop || ctx.Err() != nil {
			e.regSend.Unlock()
			return
		}
		got, err := e.sendRegister(ctx, false)
		e.regSend.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			fails = 0
			if got > 0 {
				life = got
			}
			continue
		}
		fails++
		slog.Error("REGISTER refresh failed", "err", err, "consecutive", fails)
		if fails >= 2 {
			slog.Error("REGISTER refresh failed twice; socket stays up")
			return
		}
	}
}

func (e *Endpoint) sendRegister(ctx context.Context, unregister bool) (time.Duration, error) {
	var exp *uint32
	if unregister {
		zero := uint32(0)
		exp = &zero
	} else if e.cfg.Register != nil && e.cfg.Register.Expires > 0 {
		secs := secondsOf(e.cfg.Register.Expires)
		exp = &secs
	}
	req, err := e.registerRequest(exp)
	if err != nil {
		return 0, err
	}
	res, err := e.regRoundTrip(ctx, req)
	if err != nil {
		return 0, err
	}
	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = e.answerRegisterChallenge(ctx, req, res)
		if err != nil {
			return 0, err
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return 0, fmt.Errorf("REGISTER rejected: %d %s", res.StatusCode, phrase(res.StatusCode))
	}
	if unregister {
		return 0, nil
	}
	requested := time.Duration(0)
	if e.cfg.Register != nil {
		requested = e.cfg.Register.Expires
	}
	return agreedLifetime(res, requested)
}

func (e *Endpoint) answerRegisterChallenge(ctx context.Context, req *sip.Request, res *sip.Response) (*sip.Response, error) {
	if e.authPass() == "" {
		return nil, errors.New("REGISTER challenge has no credentials")
	}
	name := "WWW-Authenticate"
	if res.StatusCode == sip.StatusProxyAuthRequired {
		name = "Proxy-Authenticate"
	}
	h := res.GetHeader(name)
	if h == nil {
		return nil, errors.New("REGISTER challenge is missing")
	}
	chal, err := digest.ParseChallenge(h.Value())
	if err != nil {
		return nil, err
	}
	if chal.Realm != e.cfg.Domain {
		return nil, fmt.Errorf("REGISTER challenge realm %q", chal.Realm)
	}
	tx, err := e.sipCli.TransactionDigestAuth(ctx, req, res, sipgo.DigestAuth{
		Username: e.authUser(),
		Password: e.authPass(),
	})
	// The helper increments CSeq before it transmits. Record that value even
	// when the send then times out, so the next REGISTER does not reuse it.
	if cseq := req.CSeq(); cseq != nil {
		e.noteRegCSeq(cseq.SeqNo)
	}
	if tx != nil {
		defer tx.Terminate()
	}
	if err != nil {
		return nil, err
	}
	got, err := waitClientTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if got.StatusCode == sip.StatusUnauthorized || got.StatusCode == sip.StatusProxyAuthRequired {
		return nil, fmt.Errorf("REGISTER challenge rejected: %d", got.StatusCode)
	}
	return got, nil
}

func (e *Endpoint) registerRequest(expires *uint32) (*sip.Request, error) {
	uri, err := e.registrarURI()
	if err != nil {
		return nil, err
	}
	e.regMu.Lock()
	if e.regCallID == "" {
		e.regCallID = newCallID()
	}
	if e.regFrom == nil {
		e.regFrom = e.localFrom()
	}
	e.regCSeq++
	seq := e.regCSeq
	callID := e.regCallID
	from := sip.HeaderClone(e.regFrom)
	e.regMu.Unlock()

	req := sip.NewRequest(sip.REGISTER, uri)
	req.SetTransport("UDP")
	req.AppendHeader(e.via())
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: e.cfg.User, Host: e.cfg.Domain}})
	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.REGISTER})
	max := sip.MaxForwardsHeader(70)
	req.AppendHeader(&max)
	req.AppendHeader(sip.HeaderClone(&e.contact))
	if expires != nil {
		req.AppendHeader(sip.NewHeader("Expires", strconv.FormatUint(uint64(*expires), 10)))
	}
	req.SetBody(nil)
	dest := uri.HostPort()
	if uri.Port == 0 {
		dest = net.JoinHostPort(uri.Host, "5060")
	}
	req.SetDestination(dest)
	req.Laddr = e.listen
	return req, nil
}

func (e *Endpoint) registrarURI() (sip.Uri, error) {
	raw := "sip:" + e.cfg.Domain
	if e.cfg.Register != nil {
		if reg := strings.TrimSpace(e.cfg.Register.Registrar); reg != "" {
			raw = reg
		}
	}
	return parseTarget(raw)
}

func (e *Endpoint) noteRegCSeq(n uint32) {
	e.regMu.Lock()
	if n > e.regCSeq {
		e.regCSeq = n
	}
	e.regMu.Unlock()
}

func (e *Endpoint) regRoundTrip(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	tx, err := e.beginTx(ctx, req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	return waitClientTx(ctx, tx)
}

func waitClientTx(ctx context.Context, tx sip.ClientTransaction) (*sip.Response, error) {
	for {
		select {
		case res := <-tx.Responses():
			if res == nil || res.IsProvisional() {
				continue
			}
			return res, nil
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("REGISTER transaction ended")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func agreedLifetime(res *sip.Response, requested time.Duration) (time.Duration, error) {
	if res != nil {
		if c := res.Contact(); c != nil && c.Params != nil {
			if v, ok := c.Params.Get("expires"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
					return time.Duration(n) * time.Second, nil
				}
			}
		}
		if h := res.GetHeader("Expires"); h != nil {
			field := strings.TrimSpace(strings.Split(h.Value(), ";")[0])
			if n, err := strconv.Atoi(field); err == nil && n > 0 {
				return time.Duration(n) * time.Second, nil
			}
		}
	}
	if requested > 0 {
		return requested, nil
	}
	return 0, errors.New("registrar expires is missing")
}

func secondsOf(d time.Duration) uint32 {
	return uint32(sessionSeconds(d))
}
