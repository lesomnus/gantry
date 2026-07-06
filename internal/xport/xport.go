// Package xport builds the outbound HTTP transport a registry store uses. It is
// the single source of truth for TPM-backed mTLS, private-CA server
// verification, and self-signed skip-verify, shared by every path that reaches a
// registry — the copy path (ggcr), the referrer/verify path (oras), and the
// health probe — so they cannot drift and leave one path unauthenticated.
package xport

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/tpm"
	"github.com/lesomnus/z"
)

// Transport returns the outbound transport for a store:
//   - a TPM-backed mTLS transport when the store configures a TPM key,
//   - a server-verification transport when ca_cert (trust a private CA) and/or
//     insecure (skip verification) is set,
//   - nil to fall back to the caller library's default transport.
//
// Both the TPM and server-TLS transports are memoized: the same store config
// yields the same *http.Transport (one open TPM device, one pooled connection
// set) across every job, layer, verification, and health probe. Only successful
// builds are cached, so a transient failure (device busy, cert mid-rotation) is
// retried on the next call rather than poisoning the store until restart.
func Transport(c config.StoreConfig) (http.RoundTripper, error) {
	if !c.HasTPM() && c.CACert == "" && !c.Insecure {
		return nil, nil // no customization; use the library default
	}

	k := key{
		device:   c.TPMDevice,
		handle:   c.TPMHandle,
		cert:     c.TPMCert,
		ca:       c.CACert,
		insecure: c.Insecure,
		tpm:      c.HasTPM(),
	}

	mu.Lock()
	if e, ok := cache[k]; ok {
		mu.Unlock()
		return e.rt, nil
	}
	kl := locks[k]
	if kl == nil {
		kl = &sync.Mutex{}
		locks[k] = kl
	}
	mu.Unlock()

	// Serialize builds for the same key so the TPM device is opened at most once,
	// without holding the global lock across the (slow) device open.
	kl.Lock()
	defer kl.Unlock()

	mu.Lock()
	if e, ok := cache[k]; ok {
		mu.Unlock()
		return e.rt, nil
	}
	mu.Unlock()

	rt, dev, err := build(c)
	if err != nil {
		return nil, err // not cached: the next call retries
	}

	mu.Lock()
	cache[k] = &entry{rt: rt, dev: dev}
	mu.Unlock()
	return rt, nil
}

// CloseTPM releases every TPM device opened for mTLS transports and clears the
// cache. Call it at shutdown; cached transports must not be used afterward.
func CloseTPM() {
	mu.Lock()
	defer mu.Unlock()
	for k, e := range cache {
		if e.dev != nil {
			_ = e.dev.Close()
		}
		delete(cache, k)
		delete(locks, k)
	}
}

// key identifies a distinct transport. Two stores sharing every field share one
// transport (and one open TPM device).
type key struct {
	device, handle, cert, ca string
	insecure, tpm            bool
}

type entry struct {
	rt  http.RoundTripper
	dev io.Closer // non-nil only for TPM transports (the device to close)
}

var (
	mu    sync.Mutex
	cache = map[key]*entry{}
	locks = map[key]*sync.Mutex{}
)

func build(c config.StoreConfig) (http.RoundTripper, io.Closer, error) {
	if c.HasTPM() {
		return buildTPMTransport(c)
	}
	rt, err := serverTLSTransport(c)
	return rt, nil, err
}

// serverTLSTransport verifies the server against ca_cert (when set) and/or skips
// verification (when insecure). It presents no client certificate.
func serverTLSTransport(c config.StoreConfig) (http.RoundTripper, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CACert != "" {
		caPEM, err := os.ReadFile(c.CACert)
		if err != nil {
			return nil, z.Err(err, "read ca_cert %q", c.CACert)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, z.Err(nil, "ca_cert has no valid certificate")
		}
		tc.RootCAs = pool
	}
	tc.InsecureSkipVerify = c.Insecure

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tc
	return t, nil
}

func buildTPMTransport(c config.StoreConfig) (http.RoundTripper, io.Closer, error) {
	handle, err := c.TPMHandleValue()
	if err != nil {
		return nil, nil, err
	}
	signer, dev, err := tpm.OpenSigner(c.TPMDevice, handle)
	if err != nil {
		return nil, nil, err
	}
	certPEM, err := os.ReadFile(c.TPMCert)
	if err != nil {
		_ = dev.Close()
		return nil, nil, z.Err(err, "read tpm_cert %q", c.TPMCert)
	}
	var caPEM []byte
	if c.CACert != "" {
		if caPEM, err = os.ReadFile(c.CACert); err != nil {
			_ = dev.Close()
			return nil, nil, z.Err(err, "read ca_cert %q", c.CACert)
		}
	}
	rt, err := mtlsTransport(certPEM, caPEM, signer, c.Insecure)
	if err != nil {
		_ = dev.Close()
		return nil, nil, err
	}
	return rt, dev, nil
}

// mtlsTransport builds an HTTPS transport that presents cert (leaf + chain) with
// signer as the private key. signer is any crypto.Signer — the TPM signer in
// production, an in-memory key in tests — so the private key material need never
// be present. caPEM (optional) verifies the server; insecure skips verification.
func mtlsTransport(certPEM, caPEM []byte, signer crypto.Signer, insecure bool) (*http.Transport, error) {
	chain, leaf, err := parseCertChain(certPEM)
	if err != nil {
		return nil, err
	}
	if err := publicKeyMatches(leaf.PublicKey, signer.Public()); err != nil {
		return nil, err
	}

	tc := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: chain,
			PrivateKey:  signer,
			Leaf:        leaf,
		}},
		MinVersion: tls.VersionTLS12,
	}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, z.Err(nil, "ca_cert has no valid certificate")
		}
		tc.RootCAs = pool
	}
	tc.InsecureSkipVerify = insecure

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tc
	return t, nil
}

// parseCertChain decodes every CERTIFICATE block from a PEM file (leaf first,
// then any intermediates) and parses the leaf.
func parseCertChain(pemBytes []byte) (chain [][]byte, leaf *x509.Certificate, err error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		chain = append(chain, block.Bytes)
	}
	if len(chain) == 0 {
		return nil, nil, z.Err(nil, "tpm_cert has no CERTIFICATE block")
	}
	leaf, err = x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, nil, z.Err(err, "parse tpm_cert leaf")
	}
	return chain, leaf, nil
}

// publicKeyMatches fails when the certificate's public key is not the public
// half of signer — i.e. tpm_cert was issued for a different key than the one at
// the configured handle. Caught here it is a clear config error rather than an
// opaque TLS handshake failure at pull time.
func publicKeyMatches(certPub, signerPub any) error {
	a, err := x509.MarshalPKIXPublicKey(certPub)
	if err != nil {
		return z.Err(err, "marshal tpm_cert public key")
	}
	b, err := x509.MarshalPKIXPublicKey(signerPub)
	if err != nil {
		return z.Err(err, "marshal TPM public key")
	}
	if !bytes.Equal(a, b) {
		return z.Err(nil, "tpm_cert public key does not match the key at the TPM handle")
	}
	return nil
}
