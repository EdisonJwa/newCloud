package main

// A TLS reverse proxy for the UI check.
//
// It is not a convenience: the session cookie carries the __Host- prefix, which
// browsers only store over HTTPS. That restriction is a real security property (it
// stops a sibling subdomain from setting the cookie), so the check must run over TLS
// rather than weaken the application for testability. Terminating TLS here keeps the
// server binary unchanged.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// startTLSProxy listens on addr and forwards to target over plain HTTP.
//
// The certificate is generated per run and trusted only by the Chrome instance this
// tool launches: nothing is installed into a system trust store.
func startTLSProxy(addr, target string) (*http.Server, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, err
	}

	upstream, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	// The default director rewrites Host to the upstream address. That must not
	// happen here: the application's OIDC callback check compares Host against its
	// configured public URL, so rewriting it would make the check verify something
	// other than what production does.
	inner := proxy.Director
	proxy.Director = func(r *http.Request) {
		host := r.Host
		inner(r)
		r.Host = host
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tls proxy: %w", err)
	}

	srv := &http.Server{
		Handler:   proxy,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go srv.ServeTLS(ln, "", "")
	return srv, nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		// Self-signed: it is its own issuer.
		IsCA: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
