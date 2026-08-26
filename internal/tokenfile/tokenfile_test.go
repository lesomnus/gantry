package tokenfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// write replaces the file the way a token publisher should: a temporary file
// and a rename, so a reader never sees a half-written credential.
//
// The timestamp is nudged because these replacements happen faster than a real
// publisher's and filesystem stamps have a granularity — the change has to be
// visible for the reason it would be in production, not by accident of timing.
func write(t *testing.T, path string, token string) {
	t.Helper()

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename onto %s: %v", path, err)
	}

	now := time.Now()
	if err := os.Chtimes(path, now, now.Add(time.Duration(len(token))*time.Millisecond)); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func token(t *testing.T, s *Source) string {
	t.Helper()

	cfg, err := s.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}

	return cfg.RegistryToken
}

// The whole reason this is a file: a credential that expires is replaced on
// disk by whatever mints it, and a gantry that read it once at startup would
// hold a dead token until somebody restarted it.
func TestReReadWhenChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "first")

	s := New(path)
	if got := token(t, s); got != "first" {
		t.Fatalf("token = %q, want first", got)
	}

	// Asked again with nothing changed.
	if got := token(t, s); got != "first" {
		t.Fatalf("token = %q, want first", got)
	}

	write(t, path, "second")

	if got := token(t, s); got != "second" {
		t.Errorf("token = %q, want second: a replaced token was not picked up", got)
	}
}

// RegistryToken and not Password, because ggcr sends that field as
// `Authorization: Bearer` whichever scheme the registry challenged with — which
// is what a registry with no bearer-token endpoint of its own still wants.
func TestItIsABearerToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "abc")

	cfg, err := New(path).Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}

	if cfg.RegistryToken != "abc" {
		t.Errorf("RegistryToken = %q", cfg.RegistryToken)
	}
	if cfg.Username != "" || cfg.Password != "" || cfg.Auth != "" {
		t.Errorf("basic fields are set: %+v", cfg)
	}
}

// A token written by `echo` or a heredoc carries a newline, and a header with
// one in it is refused for a reason nobody guesses from the message.
func TestSurroundingWhitespaceIsNotPartOfIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "  abc\n")

	if got := token(t, New(path)); got != "abc" {
		t.Errorf("token = %q, want abc", got)
	}
}

// A publisher replacing the file is briefly a file that cannot be read, and a
// pull failing for that is a pull failing because its credential was being
// renewed. Once a token has been read, a later failure is not reported.
func TestAFailedReadKeepsWhatItHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "first")

	s := New(path)
	if got := token(t, s); got != "first" {
		t.Fatalf("token = %q, want first", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := token(t, s); got != "first" {
		t.Errorf("token = %q: a token already read should survive the file going away", got)
	}

	write(t, path, "second")

	if got := token(t, s); got != "second" {
		t.Errorf("token = %q, want second", got)
	}
}

// The other half of the case above: with nothing to fall back on there is no
// credential to send, so the caller hears about it rather than sending an empty
// one and reading the registry's 401.
func TestNothingToFallBackOnIsAnError(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		what string
		set  func(t *testing.T) string
	}{
		{"no such file", func(t *testing.T) string { return filepath.Join(dir, "absent") }},
		{"empty", func(t *testing.T) string {
			p := filepath.Join(dir, "empty")
			if err := os.WriteFile(p, []byte("   \n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"not a credential at all", func(t *testing.T) string {
			p := filepath.Join(dir, "huge")
			if err := os.WriteFile(p, []byte(strings.Repeat("x", 128<<10)), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if _, err := New(tc.set(t)).Authorization(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A copy is many requests at once and each asks for the credential, so this is
// read concurrently by construction. Run with -race.
func TestConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "shared")

	s := New(path)

	var wg sync.WaitGroup
	out := make([]string, 64)
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if cfg, err := s.Authorization(); err == nil {
				out[i] = cfg.RegistryToken
			}
		}()
	}
	wg.Wait()

	for i, got := range out {
		if got != "shared" {
			t.Fatalf("goroutine %d saw %q", i, got)
		}
	}
}
