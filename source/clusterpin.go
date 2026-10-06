package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Cluster nodes identify themselves with a self-made certificate whose SHA-256
// fingerprint is pinned (it travels in the join code and in the member list).
// There is no CA, so the usual chain check cannot work; but the certificate is
// still verified properly, never skipped: the first connection to a fingerprint
// only learns the certificate the peer offers (the standard verification fails
// with it attached), accepts it only if its fingerprint is the pinned one, and
// the real connection then verifies the peer against exactly that certificate.

// peerServerName is the one name every node identity certificate carries
// (cluster.go); the URL host is never used, the dial goes to the peer's address.
const peerServerName = "ddgw-node"

var errPeerIdentity = errors.New("peer identity does not match the pinned fingerprint")

// pinnedCerts maps a fingerprint to the certificate that has it.
var pinnedCerts sync.Map // string -> *x509.Certificate

func certFingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func fpEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// learnPinnedCert returns the certificate with fingerprint fp that the peer at
// addr offers, remembering it.
func learnPinnedCert(ctx context.Context, addr, fp string) (*x509.Certificate, error) {
	if v, ok := pinnedCerts.Load(fp); ok {
		return v.(*x509.Certificate), nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{ServerName: peerServerName, MinVersion: tls.VersionTLS12})
	err = tc.HandshakeContext(ctx)
	var certs []*x509.Certificate
	var cve *tls.CertificateVerificationError
	switch {
	case err == nil:
		certs = tc.ConnectionState().PeerCertificates
	case errors.As(err, &cve):
		certs = cve.UnverifiedCertificates
	default:
		return nil, err
	}
	if len(certs) == 0 || !fpEqual(certFingerprint(certs[0]), fp) {
		return nil, errPeerIdentity
	}
	pinnedCerts.Store(fp, certs[0])
	return certs[0], nil
}

// dialPinned opens a TLS connection to addr that is verified against the
// certificate pinned by fp.
func dialPinned(ctx context.Context, addr, fp string) (net.Conn, error) {
	return dialPinnedAs(ctx, addr, fp, nil)
}

// dialPinnedAs is dialPinned presenting our own identity certificate (when not nil) to the peer, which checks
// it against the identity we claim in the request (Cluster.peerAuth).
func dialPinnedAs(ctx context.Context, addr, fp string, me *tls.Certificate) (net.Conn, error) {
	if !validHostPort(addr) {
		return nil, errors.New("invalid peer address")
	}
	cert, err := learnPinnedCert(ctx, addr, fp)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{ServerName: peerServerName, RootCAs: pool, MinVersion: tls.VersionTLS12}
	if me != nil {
		cfg.Certificates = []tls.Certificate{*me}
	}
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		pinnedCerts.Delete(fp)
		return nil, err
	}
	if st := tc.ConnectionState(); len(st.PeerCertificates) == 0 || !fpEqual(certFingerprint(st.PeerCertificates[0]), fp) {
		conn.Close()
		return nil, errPeerIdentity
	}
	return tc, nil
}

// pinnedClient is an HTTP client for the peer at addr with fingerprint fp.
// Requests are made to https://ddgw-node/...; the connection goes to addr.
func pinnedClient(addr, fp string, timeout time.Duration) *http.Client {
	return pinnedClientAs(addr, fp, timeout, nil)
}

// pinnedClientAs is pinnedClient presenting our own identity certificate to the peer.
func pinnedClientAs(addr, fp string, timeout time.Duration, me *tls.Certificate) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialPinnedAs(ctx, addr, fp, me)
			},
		},
	}
}

// peerURL is the URL for a request to a peer made with pinnedClient.
func peerURL(path string) string { return "https://" + peerServerName + path }
