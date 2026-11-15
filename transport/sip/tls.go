package sip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// serverTLSConfig is the listener certificate. Files win when both are set.
// SelfSigned generates a certificate for name when the files are empty.
func serverTLSConfig(cfg Config, name string) (*tls.Config, error) {
	cert, err := serverCertificate(cfg, name)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func serverCertificate(cfg Config, name string) (tls.Certificate, error) {
	if cfg.TLS == nil {
		return tls.Certificate{}, errors.New("tls needs a certificate")
	}
	if cfg.TLS.CertFile != "" || cfg.TLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("tls certificate: %w", err)
		}
		return cert, nil
	}
	if !cfg.TLS.SelfSigned {
		return tls.Certificate{}, errors.New("tls needs a certificate")
	}
	cert, err := selfSignedCertificate(name)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls certificate: %w", err)
	}
	return cert, nil
}

// clientTLSConfig is the outbound policy. Verification stays on unless
// IgnoreCertWarnings is set, in which case a peer certificate that fails
// verification is accepted, including a self-signed peer.
func clientTLSConfig(cfg *TLSConfig) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg != nil && cfg.IgnoreCertWarnings {
		c.InsecureSkipVerify = true
	}
	return c
}

func selfSignedCertificate(name string) (tls.Certificate, error) {
	if name == "" {
		return tls.Certificate{}, errors.New("tls certificate name is empty")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
