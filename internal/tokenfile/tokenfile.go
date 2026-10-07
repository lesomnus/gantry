// Package tokenfile is a bearer token that lives in a file and is re-read when
// it changes.
//
// It is here rather than beside one of its callers because three of them reach
// a registry by three different libraries -- ggcr for blobs, oras for referrers
// and for signature verification -- and a credential that only one of them
// understood would be a store that can pull an image and cannot check its
// signature.
package tokenfile

import (
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/lesomnus/xli/cfg"
)

// Source is a bearer token read from disk, re-read when the file changes.
//
// # Why a file, and why re-read
//
// The credential this exists for **expires**. Something else on the host mints
// it — against a device certificate, on a schedule of its own — and drops the
// new one where this can see it. gantry's other credentials are values in the
// configuration, expanded once when it starts, which is right for a password
// that does not change and useless for one that does: the process would hold a
// dead token until somebody restarted it.
//
// So the file is the interface, and the only thing gantry has to do is notice.
//
// # Noticing is a stat, not a watcher
//
// [authn.Authenticator] is asked for the credential **per request**, so there
// is already a moment to check on, every time it could matter. Checking there
// costs one `stat` of a small file next to a blob transfer, and it has no
// staleness window: there is no interval to tune and no period during which a
// replaced token is known and not used.
//
// # The rules are cfg's
//
// The file is read as `${file:path}` is in a configuration xli's cfg reads: a
// [cfg.SecretOf]. Those rules were this package's before they were cfg's --
// cr, gantry and bosun each held a copy, and bosun's rotation test failing one
// run in three (Holiday-Robot/bosun#19) is how they came to agree -- so there
// is one copy now, and these are its terms:
//
//   - the FILE is compared, and then its size and modification time: a rename,
//     which is how a token should be published, always puts a different file
//     at the path, while every bearer one issuer mints is the same length and
//     two writes inside one tick of a coarse clock carry the same time;
//   - a read that fails after a good one keeps the token in hand, since a
//     publisher replacing the file is briefly a file that cannot be read;
//   - an empty file is a failed read, and so is one over 64 KiB or one that is
//     not a regular file.
//
// The whitespace around the token is not part of it (cfg.TrimSpaceDecoder): a
// token written by `echo` or a heredoc has a newline, and a header with one in
// it is a header the server rejects for a reason nobody guesses.
//
// A writer that rewrites IN PLACE with the same length inside one tick is still
// not seen. That is the contract saying `rename` rather than a reason to hash
// the contents on every request, and TestAnInPlaceRewriteIsNotSeen holds it
// there so it stays a decision.
type Source struct {
	token cfg.SecretOf[string, cfg.TrimSpaceDecoder]
	// err is why the path could not be named at all.
	err error
}

var _ authn.Authenticator = (*Source)(nil)

// New is the token in this file. The file is read now, and a file that cannot
// be read yet is not an error until the token is asked for.
func New(path string) *Source {
	s := &Source{}
	s.err = s.token.SetFile(path)
	return s
}

// Authorization answers the current token, reading the file when it has changed
// since the last look. With nothing read yet to fall back on, it is an error
// naming the file: there is no credential to send.
func (t *Source) Authorization() (*authn.AuthConfig, error) {
	token, err := t.Token()
	if err != nil {
		return nil, err
	}

	// RegistryToken and not Password: ggcr sends this as `Authorization: Bearer`
	// whichever scheme the registry challenged with, which is what a registry
	// that does not implement the bearer-token endpoint still wants to receive.
	return &authn.AuthConfig{RegistryToken: token}, nil
}

// Token is the current token, for a caller that wants the string rather than
// an [authn.AuthConfig] -- oras carries it in a credential of its own shape.
func (t *Source) Token() (string, error) {
	if t.err != nil {
		return "", t.err
	}

	return t.token.Value()
}
