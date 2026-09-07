package xport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// issueClientCertValid issues a client certificate with an explicit validity
// window, which is the whole subject here.
func issueClientCertValid(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, notBefore, notAfter time.Time) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

// A peer that will not accept our client certificate says only "remote error:
// tls: expired certificate", and that sentence sends people to renew a
// certificate that is not the problem. This is the case that motivated the
// annotation: the certificate is valid by our own clock and was issued minutes
// ago, so the peer must disagree about the time — but the alert cannot say so,
// because TLS sends the same one for "not yet valid".
func TestCertAlertCarriesWhatWePresented(t *testing.T) {
	ca, caKey, caPEM := genCA(t)
	serverCertPEM, serverKey := issueCert(t, ca, caKey, "server", true)
	serverCert, err := tls.X509KeyPair(serverCertPEM, encodeKey(t, serverKey))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA")
	}

	// Not yet valid, which is how a peer whose clock is behind sees a
	// certificate that is perfectly valid here. The peer answers alert 45.
	notBefore := time.Now().Add(30 * time.Minute)
	clientCertPEM, clientKey := issueClientCertValid(t, ca, caKey, notBefore, notBefore.Add(24*time.Hour))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the server's own complaint is not the subject
	srv.StartTLS()
	defer srv.Close()

	rt, err := mtlsTransport("the key at the TPM handle", "/etc/gantry/docker-client.crt", clientCertPEM, caPEM, clientKey, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: rt}).Get(srv.URL)
	if err == nil {
		t.Fatal("the server must refuse a certificate it considers invalid")
	}
	msg := err.Error()
	for _, want := range []string{
		"remote error",                  // the peer's own words are kept
		"/etc/gantry/docker-client.crt", // and which file they are about
		notBefore.UTC().Format(time.RFC3339),
		"now ",
		"does not become valid for another", // our clock agrees it is early
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

// The annotation is only for the peer refusing a certificate. Everything else —
// a refused connection, a timeout, an HTTP error — must come back untouched,
// or every unrelated failure grows a paragraph about certificates.
func TestOnlyCertificateAlertsAreAnnotated(t *testing.T) {
	ca, caKey, caPEM := genCA(t)
	certPEM, key := issueCert(t, ca, caKey, "client", false)
	rt, err := mtlsTransport("cred.key", "cred.cert", certPEM, caPEM, key, false)
	if err != nil {
		t.Fatal(err)
	}
	// Port 0 is never listening.
	_, err = (&http.Client{Transport: rt}).Get("https://127.0.0.1:0/")
	if err == nil {
		t.Fatal("want a dial failure")
	}
	if strings.Contains(err.Error(), "we presented") {
		t.Errorf("a dial failure was annotated as a certificate problem:\n%s", err)
	}
}

// The window reported is the one the whole chain agrees on: an intermediate
// that expires first, or is issued last, is what actually bounds the handshake.
func TestChainWindowIsTheNarrowestOne(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ca, caKey, _ := genCA(t)
	leafPEM, _ := issueClientCertValid(t, ca, caKey, now.Add(-2*time.Hour), now.Add(48*time.Hour))
	interPEM, _ := issueClientCertValid(t, ca, caKey, now.Add(-time.Hour), now.Add(2*time.Hour))

	chain, _, err := parseCertChain(append(append([]byte{}, leafPEM...), interPEM...))
	if err != nil {
		t.Fatal(err)
	}
	w := chainWindow(chain)
	if !w.notBefore.Equal(now.Add(-time.Hour).UTC().Truncate(time.Second)) {
		t.Errorf("notBefore = %v, want the latest one (%v)", w.notBefore, now.Add(-time.Hour))
	}
	if !w.notAfter.Equal(now.Add(2 * time.Hour).UTC().Truncate(time.Second)) {
		t.Errorf("notAfter = %v, want the earliest one (%v)", w.notAfter, now.Add(2*time.Hour))
	}
}

// What the message says has to follow from the clock, since the whole point is
// to tell a reader which half of the problem is theirs.
func TestExplainSaysWhichSideTheClockIsOn(t *testing.T) {
	base := time.Date(2026, 9, 7, 4, 21, 10, 0, time.UTC)
	a := &certAnnotator{
		name: "/opt/hday/robot/docker-client.crt",
		win:  certWindow{notBefore: base, notAfter: base.AddDate(40, 0, 0)},
	}
	for _, tt := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"issued minutes ago", base.Add(7*time.Minute + 39*time.Second), "became valid 7m39s ago"},
		{"not yet valid here too", base.Add(-3 * time.Hour), "does not become valid for another 3h0m0s"},
		{"actually expired", base.AddDate(41, 0, 0), "expired"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := a.explain(tt.now)
			if !strings.Contains(got, tt.want) {
				t.Errorf("explain = %q, want it to mention %q", got, tt.want)
			}
			if !strings.Contains(got, a.name) {
				t.Errorf("explain does not name the certificate: %q", got)
			}
		})
	}
}
