package sip

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSignedTLSHandshake(t *testing.T) {
	cfg := baseConfig()
	cfg.Transports = []string{"tls"}
	cfg.TLS = &TLSConfig{SelfSigned: true}
	srv := startEP(t, cfg)
	if srv.sipTLS == nil {
		t.Fatal("tls listener did not start")
	}
	addr := srv.sipTLS.Addr().String()
	a, err := srv.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	name := a.AdvertiseIP.String()

	ok := startEP(t, tlsClientConfig(true))
	if ok.tlsClient == nil || !ok.tlsClient.InsecureSkipVerify {
		t.Fatal("ignore-warnings client did not disable verification")
	}
	conn, err := dialTLS(ok.tlsClient, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("handshake did not complete")
	}
	if len(state.PeerCertificates) == 0 {
		t.Fatal("peer certificate missing")
	}
	leaf := state.PeerCertificates[0]
	if leaf.Subject.CommonName != name {
		t.Fatalf("certificate name %q, advertised %q", leaf.Subject.CommonName, name)
	}
	if !bytes.Equal(leaf.RawIssuer, leaf.RawSubject) {
		t.Fatal("peer certificate is not self-signed")
	}

	bad := startEP(t, tlsClientConfig(false))
	if bad.tlsClient == nil || bad.tlsClient.InsecureSkipVerify {
		t.Fatal("verification was not left on")
	}
	if _, err := dialTLS(bad.tlsClient, addr); err == nil {
		t.Fatal("handshake succeeded against a self-signed certificate")
	} else {
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			t.Fatalf("handshake error %v, want an unknown authority", err)
		}
	}
}

func TestUDPandTLSBothListen(t *testing.T) {
	cfg := baseConfig()
	cfg.Transports = []string{"udp", "tls"}
	cfg.TLS = &TLSConfig{SelfSigned: true}
	ep := startEP(t, cfg)
	if ep.sipUDP == nil || ep.sipTLS == nil {
		t.Fatal("udp and tls were not both listening")
	}
	udpAddr, ok := ep.sipUDP.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("udp address missing")
	}
	if ep.sipTLS.Addr().String() == "" || udpAddr.Port == 0 {
		t.Fatal("listen address missing")
	}
}

func TestTLSCertificateFiles(t *testing.T) {
	certPath, keyPath := writeCertFiles(t, "files.example")
	cfg := baseConfig()
	cfg.Transports = []string{"tls"}
	cfg.TLS = &TLSConfig{CertFile: certPath, KeyFile: keyPath, SelfSigned: true}
	srv := startEP(t, cfg)
	client := startEP(t, tlsClientConfig(true))
	conn, err := dialTLS(client.tlsClient, srv.sipTLS.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	leaf := conn.ConnectionState().PeerCertificates[0]
	if leaf.Subject.CommonName != "files.example" {
		t.Fatalf("certificate name %q", leaf.Subject.CommonName)
	}
}

func TestTLSNeedsCertificate(t *testing.T) {
	cfg := baseConfig()
	cfg.Transports = []string{"tls"}
	cfg.SIPPort = freePort(t)
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = ep.Start(context.Background())
	if err == nil || err.Error() != "tls needs a certificate" {
		t.Fatalf("Start err %v", err)
	}
}

func tlsClientConfig(ignore bool) Config {
	cfg := baseConfig()
	cfg.TLS = &TLSConfig{IgnoreCertWarnings: ignore}
	return cfg
}

func dialTLS(cfg *tls.Config, addr string) (*tls.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	c := cfg.Clone()
	if c.ServerName == "" {
		c.ServerName = host
	}
	d := &tls.Dialer{Config: c}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		conn.Close()
		return nil, errors.New("dial did not return a TLS connection")
	}
	return tlsConn, nil
}

func writeCertFiles(t *testing.T, name string) (string, string) {
	t.Helper()
	cert, err := selfSignedCertificate(name)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
