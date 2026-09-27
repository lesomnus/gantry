package cpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

func registryStore(host string) config.StoreConfig {
	return config.StoreConfig{Name: "cache", Kind: "oci", Host: host, Insecure: true, Mode: "proxy"}
}

func resolves(t *testing.T, ref string) bool {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Head(r)
	return err == nil
}

// A digest names the manifest, and that is what goes.
func TestRemoveFromRegistryDeletesTheManifestADigestNames(t *testing.T) {
	host := startRegistry(t)
	img := pushImage(t, host+"/dist/app:1", 1)
	desc, err := remote.Head(img)
	if err != nil {
		t.Fatal(err)
	}
	dg := desc.Digest.String()

	res, err := RemoveFromRegistry(context.Background(), registryStore(host), "dist/app@"+dg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != dg || len(res.Untagged) != 0 {
		t.Errorf("result = %+v, want the one manifest %s deleted", res, dg)
	}
	if resolves(t, host+"/dist/app@"+dg) {
		t.Error("the manifest still resolves after it was deleted")
	}
}

// A tag names the tag. Whether the registry allows that is its own business —
// the in-memory one does — and the manifest behind it is not asked about.
func TestRemoveFromRegistryDeletesTheTagATagNames(t *testing.T) {
	host := startRegistry(t)
	pushImage(t, host+"/dist/app:1", 1)

	res, err := RemoveFromRegistry(context.Background(), registryStore(host), "dist/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Untagged) != 1 || res.Untagged[0] != host+"/dist/app:1" || len(res.Deleted) != 0 {
		t.Errorf("result = %+v, want the tag untagged and no manifest deleted", res)
	}
	if resolves(t, host+"/dist/app:1") {
		t.Error("the tag still resolves after it was deleted")
	}
}

// The ref as a node spells it — with the cache's host in front — is the same
// image, so it is accepted rather than read as a repository named after a host.
func TestRemoveFromRegistryAcceptsTheStoresOwnHost(t *testing.T) {
	host := startRegistry(t)
	pushImage(t, host+"/dist/app:1", 1)

	if _, err := RemoveFromRegistry(context.Background(), registryStore(host), host+"/dist/app:1"); err != nil {
		t.Fatal(err)
	}
	if resolves(t, host+"/dist/app:1") {
		t.Error("the tag still resolves after it was deleted")
	}
}

func TestRemoveFromRegistryReportsWhatIsNotThere(t *testing.T) {
	host := startRegistry(t)
	pushImage(t, host+"/dist/app:1", 1)

	_, err := RemoveFromRegistry(context.Background(), registryStore(host),
		"dist/app@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if !errors.Is(err, ErrNoSuchImage) {
		t.Errorf("err = %v, want ErrNoSuchImage", err)
	}
}

func TestRemoveFromRegistryRejectsAMalformedRef(t *testing.T) {
	_, err := RemoveFromRegistry(context.Background(), registryStore("127.0.0.1:1"), "Dist/App:1")
	if !errors.Is(err, ErrInvalidReference) {
		t.Errorf("err = %v, want ErrInvalidReference", err)
	}
}

func TestRemoveFromRegistryRefusesAnEngineStore(t *testing.T) {
	_, err := RemoveFromRegistry(context.Background(), config.StoreConfig{Name: "node", Kind: "docker"}, "dist/app:1")
	if err == nil {
		t.Fatal("an engine store was accepted")
	}
}

// A registry's refusals are told apart, because the caller can do something
// different about each: a registry that does no deletes will never do one, and
// a credential that may not delete needs a binding, not a retry.
func TestRemoveFromRegistrySortsTheRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"distribution with deletes off", http.StatusMethodNotAllowed,
			`{"errors":[{"code":"UNSUPPORTED","message":"The operation is unsupported."}]}`, ErrDeleteUnsupported},
		{"unsupported under another status", http.StatusBadRequest,
			`{"errors":[{"code":"UNSUPPORTED","message":"tag deletes are not supported"}]}`, ErrDeleteUnsupported},
		{"forbidden", http.StatusForbidden,
			`{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`, ErrDeleteDenied},
		{"unauthorized", http.StatusUnauthorized,
			`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, ErrDeleteDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			u, _ := url.Parse(srv.URL)

			_, err := RemoveFromRegistry(context.Background(), registryStore(u.Host),
				"dist/app@sha256:0000000000000000000000000000000000000000000000000000000000000000")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
