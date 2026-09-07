package xport

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// certAnnotator adds what gantry knows about the certificate it presented to a
// peer's certificate alert.
//
// A remote alert carries no detail — "remote error: tls: expired certificate"
// is the whole of it — and that phrase reads as an instruction to renew a
// certificate that may have been issued minutes ago. The other explanation is
// the peer's clock, and neither TLS nor Go can tell the two apart from this
// side: x509 reports "not yet valid" and "expired" as the same Expired reason,
// and alert 45 (certificate_expired) is sent for both. A peer whose clock is
// behind therefore rejects a brand-new certificate with the word "expired".
//
// So the error says what we can see: the window of the chain we presented,
// where it came from, and our own clock. That does not name the cause, but it
// rules out the half a reader would otherwise check first — and a notBefore a
// few minutes old is itself the tell.
type certAnnotator struct {
	inner http.RoundTripper
	name  string     // where the certificate came from (the cred.cert path)
	win   certWindow // what the presented chain agrees on
	now   func() time.Time
}

func (a *certAnnotator) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := a.inner.RoundTrip(r)
	if err == nil || !remoteCertAlert(err) {
		return res, err
	}
	return res, fmt.Errorf("%w (%s)", err, a.explain(a.now()))
}

// certWindow is the validity the whole presented chain agrees on: the latest
// notBefore and the earliest notAfter across leaf and intermediates. One pair
// answers the question a peer's alert makes people ask — is what we presented
// valid right now — without printing a chain.
type certWindow struct {
	notBefore, notAfter time.Time
}

func chainWindow(chain [][]byte) certWindow {
	var w certWindow
	for _, der := range chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			continue // the leaf already parsed; an unreadable intermediate says nothing
		}
		if w.notBefore.IsZero() || c.NotBefore.After(w.notBefore) {
			w.notBefore = c.NotBefore
		}
		if w.notAfter.IsZero() || c.NotAfter.Before(w.notAfter) {
			w.notAfter = c.NotAfter
		}
	}
	return w
}

func (a *certAnnotator) explain(now time.Time) string {
	var verdict string
	switch {
	case a.win.notBefore.IsZero():
		verdict = "we could not read its validity"
	case now.Before(a.win.notBefore):
		verdict = fmt.Sprintf("by our clock it does not become valid for another %s",
			a.win.notBefore.Sub(now).Round(time.Second))
	case now.After(a.win.notAfter):
		verdict = fmt.Sprintf("by our clock it expired %s ago", now.Sub(a.win.notAfter).Round(time.Second))
	default:
		verdict = fmt.Sprintf("by our clock it is valid, and became valid %s ago; "+
			"a peer whose clock is behind ours rejects a not-yet-valid certificate with this same alert",
			now.Sub(a.win.notBefore).Round(time.Second))
	}
	return fmt.Sprintf("we presented %s, valid %s..%s, now %s: %s",
		a.name,
		stamp(a.win.notBefore), stamp(a.win.notAfter), now.UTC().Format(time.RFC3339),
		verdict)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.UTC().Format(time.RFC3339)
}

// remoteCertAlert reports whether err is the PEER refusing a certificate.
//
// Only the remote half needs this: a certificate the local side rejects comes
// back as an x509 error that already prints the window and the current time,
// which is exactly what an alert leaves out. The alert arrives as an
// unexported tls.alert inside a "remote error" *net.OpError — tls.AlertError
// does not match it — so its message is what identifies it.
func remoteCertAlert(err error) bool {
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" || oe.Err == nil {
		return false
	}
	return strings.Contains(oe.Err.Error(), "certificate")
}
