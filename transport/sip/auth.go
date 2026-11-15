package sip

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

type authState struct {
	chal digest.Challenge
	sent bool
}

func (e *Endpoint) authUser() string {
	if e.cfg.Auth != nil && e.cfg.Auth.Username != "" {
		return e.cfg.Auth.Username
	}
	return e.cfg.User
}

func (e *Endpoint) authPass() string {
	if e.cfg.Auth == nil {
		return ""
	}
	return e.cfg.Auth.Password
}

// checkAuth reports whether the request may proceed.
// A Call-ID is challenged once. A later failure is 403 with no new challenge.
func (e *Endpoint) checkAuth(callID string, req *sip.Request) (bool, int, sip.Header, error) {
	if !e.cfg.RequireAuth {
		return true, 0, nil, nil
	}
	st := e.dialogs.authState(callID)
	if st != nil && authMatches(st.chal, e.authUser(), e.authPass(), req) {
		return true, 0, nil, nil
	}
	if st != nil && st.sent {
		return false, sip.StatusForbidden, nil, nil
	}
	hdr, err := e.dialogs.challenge(callID, e.cfg.Domain)
	if err != nil {
		return false, 0, nil, err
	}
	return false, sip.StatusUnauthorized, hdr, nil
}

func (t *dialogTable) authState(callID string) *authState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.auth[callID]
	if st == nil {
		return nil
	}
	copy := *st
	return &copy
}

func (t *dialogTable) challenge(callID, realm string) (sip.Header, error) {
	nonce, err := freshNonce()
	if err != nil {
		return nil, err
	}
	chal := digest.Challenge{
		Realm:     realm,
		Nonce:     nonce,
		Algorithm: "MD5",
		QOP:       []string{"auth"},
	}
	t.mu.Lock()
	t.auth[callID] = &authState{chal: chal, sent: true}
	t.mu.Unlock()
	return sip.NewHeader("WWW-Authenticate", chal.String()), nil
}

func authMatches(chal digest.Challenge, user, pass string, req *sip.Request) bool {
	h := req.GetHeader("Authorization")
	if h == nil || user == "" || pass == "" {
		return false
	}
	creds, err := digest.ParseCredentials(h.Value())
	if err != nil || creds.Username != user || creds.Realm != chal.Realm || creds.Nonce != chal.Nonce {
		return false
	}
	method := "INVITE"
	if cseq := req.CSeq(); cseq != nil && cseq.MethodName != "" {
		method = string(cseq.MethodName)
	}
	got, err := digest.Digest(&chal, digest.Options{
		Method:   method,
		URI:      creds.URI,
		Username: user,
		Password: pass,
		Count:    creds.Nc,
		Cnonce:   creds.Cnonce,
	})
	if err != nil || len(got.Response) != len(creds.Response) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got.Response), []byte(creds.Response)) == 1
}

func freshNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	if hex.EncodeToString(buf[:]) == "" {
		return "", errors.New("empty nonce")
	}
	return hex.EncodeToString(buf[:]), nil
}
