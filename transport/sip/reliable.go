package sip

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// sendProvisional sends a 1xx other than 100. Require and RSeq are added,
// and the response is retransmitted, only when the INVITE offered or required 100rel.
func (d *dialog) sendProvisional(code int, reason string) error {
	if code <= 100 || code >= 200 {
		return errBadCSeq
	}
	d.mu.Lock()
	tx := d.tx
	reliable := d.reliableOK
	d.mu.Unlock()
	if tx == nil || d.sess == nil || d.sess.InviteRequest == nil {
		return errBadCSeq
	}
	res := sip.NewResponseFromRequest(d.sess.InviteRequest, code, reason, nil)
	d.ep.decorate(res, false)
	d.stamp(res, nil)
	if reliable {
		d.mu.Lock()
		d.rseq++
		rseq := d.rseq
		d.mu.Unlock()
		res.AppendHeader(sip.NewHeader("Require", "100rel"))
		res.AppendHeader(sip.NewHeader("RSeq", strconv.FormatUint(uint64(rseq), 10)))
		d.armProvisional(tx, res)
	}
	return tx.Respond(res)
}

func (d *dialog) armProvisional(tx sip.ServerTransaction, res *sip.Response) {
	d.stopProvisional()
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.provCancel = cancel
	d.mu.Unlock()
	go func() {
		timer := time.NewTimer(sip.T1)
		defer timer.Stop()
		gap := sip.T1
		for {
			select {
			case <-ctx.Done():
				return
			case <-tx.Done():
				return
			case <-timer.C:
				if err := tx.Respond(res); err != nil {
					return
				}
				gap *= 2
				if gap > sip.T2 {
					gap = sip.T2
				}
				timer.Reset(gap)
			}
		}
	}()
}

func (d *dialog) stopProvisional() {
	d.mu.Lock()
	cancel := d.provCancel
	d.provCancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *dialog) matchRAck(req *sip.Request) bool {
	h := req.GetHeader("RAck")
	if h == nil {
		return false
	}
	fields := strings.Fields(h.Value())
	if len(fields) != 3 || !strings.EqualFold(fields[2], "INVITE") {
		return false
	}
	rseq, err1 := strconv.ParseUint(fields[0], 10, 32)
	cseq, err2 := strconv.ParseUint(fields[1], 10, 32)
	if err1 != nil || err2 != nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if uint32(cseq) != d.remoteInvite || d.rseq == 0 || uint32(rseq) == 0 || uint32(rseq) > d.rseq {
		return false
	}
	if uint32(rseq) > d.pracked {
		d.pracked = uint32(rseq)
	}
	return true
}

func (e *Endpoint) onPrack(req *sip.Request, tx sip.ServerTransaction) {
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
	if !d.matchRAck(req) {
		_ = e.respond(tx, req, sip.StatusCallTransactionDoesNotExists, phrase(sip.StatusCallTransactionDoesNotExists), nil)
		return
	}
	d.stopProvisional()
	_ = e.respond(tx, req, sip.StatusOK, phrase(sip.StatusOK), nil)
}
