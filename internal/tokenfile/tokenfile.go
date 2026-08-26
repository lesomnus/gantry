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
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
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
// A watcher or a ticker would be a goroutine, a lifecycle and a set of failure
// modes -- an inotify watch that a rename breaks, a tick that fires while the
// writer is halfway through -- bought in exchange for saving a stat.
//
// # What is compared
//
// Size and modification time. A writer that replaces the file atomically
// (write-and-rename, which is how a token should be published) changes both.
// One that rewrites in place with the same length inside a filesystem
// timestamp tick would not be seen, which is a reason to say `rename` in the
// documentation rather than a reason to hash the contents on every request.
type Source struct {
	path string

	mu    sync.Mutex
	token string
	size  int64
	mod   time.Time
	read  bool
}

var _ authn.Authenticator = (*Source)(nil)

// New is the token in this file.
func New(path string) *Source { return &Source{path: path} }

// Authorization answers the current token, reading the file when it has changed
// since the last look.
//
// A read that fails **after** one has succeeded keeps the token it has and says
// nothing: a writer replacing the file is briefly a file that cannot be read,
// and a pull that failed for that would be a pull failing because a credential
// was being renewed. A read that fails with nothing to fall back on is an
// error, since there is no credential to send.
func (t *Source) Authorization() (*authn.AuthConfig, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if changed, err := t.stale(); err != nil {
		if !t.read {
			return nil, err
		}
	} else if changed {
		if err := t.reload(); err != nil && !t.read {
			return nil, err
		}
	}

	if !t.read {
		return nil, fmt.Errorf("token file %s: never read", t.path)
	}

	// RegistryToken and not Password: ggcr sends this as `Authorization: Bearer`
	// whichever scheme the registry challenged with, which is what a registry
	// that does not implement the bearer-token endpoint still wants to receive.
	return &authn.AuthConfig{RegistryToken: t.token}, nil
}

// Token is the current token, for a caller that wants the string rather than
// an [authn.AuthConfig] -- oras carries it in a credential of its own shape.
func (t *Source) Token() (string, error) {
	cfg, err := t.Authorization()
	if err != nil {
		return "", err
	}

	return cfg.RegistryToken, nil
}

// stale reports whether the file differs from what was last read.
func (t *Source) stale() (bool, error) {
	fi, err := os.Stat(t.path)
	if err != nil {
		return false, fmt.Errorf("token file %s: %w", t.path, err)
	}

	return !t.read || fi.Size() != t.size || !fi.ModTime().Equal(t.mod), nil
}

// reload reads the file and records what it was when read.
//
// The stat is taken from the same handle the contents came from, so a file
// replaced between the two is not recorded as the one that was read -- the next
// request sees a difference and reads again, rather than holding stale content
// under a fresh stamp.
func (t *Source) reload() error {
	f, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("token file %s: %w", t.path, err)
	}
	defer f.Close()

	b, err := readAllLimited(f)
	if err != nil {
		return fmt.Errorf("token file %s: %w", t.path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("token file %s: %w", t.path, err)
	}

	// Trailing whitespace, because a token written by `echo` or a heredoc has a
	// newline and a header with one in it is a header the server rejects for a
	// reason nobody guesses.
	token := strings.TrimSpace(string(b))
	if token == "" {
		return fmt.Errorf("token file %s: empty", t.path)
	}

	t.token, t.size, t.mod, t.read = token, fi.Size(), fi.ModTime(), true

	return nil
}

// maxTokenFile is what will be read from a token file.
//
// A JWS is a few kilobytes at most. The limit is here because this path reads a
// file whose contents somebody else writes, on every request, and a file that
// is not a token should be an error rather than however much memory it is.
const maxTokenFile = 64 << 10

func readAllLimited(f *os.File) ([]byte, error) {
	b := make([]byte, maxTokenFile+1)
	n, err := f.Read(b)
	if err != nil && n == 0 {
		return nil, err
	}
	if n > maxTokenFile {
		return nil, fmt.Errorf("larger than %d bytes", maxTokenFile)
	}

	return b[:n], nil
}
