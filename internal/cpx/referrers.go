package cpx

import (
	"context"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/z"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// keychainCredential resolves credentials from the docker keychain, matching
// the copy path's authn.DefaultKeychain fallback.
func keychainCredential(repo name.Repository) func(context.Context, string) (auth.Credential, error) {
	return func(context.Context, string) (auth.Credential, error) {
		a, err := authn.DefaultKeychain.Resolve(repo.Registry)
		if err != nil {
			return auth.EmptyCredential, nil
		}
		cfg, err := a.Authorization()
		if err != nil || cfg == nil {
			return auth.EmptyCredential, nil
		}
		return auth.Credential{
			Username:     cfg.Username,
			Password:     cfg.Password,
			RefreshToken: cfg.IdentityToken,
			AccessToken:  cfg.RegistryToken,
		}, nil
	}
}

// orasRepo builds an oras repository for a registry store with the same wiring
// as the ggcr copy path: basic auth when credentials are set, else the docker
// keychain; insecure covers both plain-HTTP and self-signed registries.
func orasRepo(c config.StoreConfig, repo name.Repository) (*remote.Repository, error) {
	r, err := remote.NewRepository(repo.Name())
	if err != nil {
		return nil, err
	}
	client := &auth.Client{Cache: auth.NewCache()}
	if c.Username != "" {
		client.Credential = auth.StaticCredential(repo.RegistryStr(),
			auth.Credential{Username: c.Username, Password: c.Password})
	} else {
		client.Credential = keychainCredential(repo)
	}
	// Same outbound transport as the ggcr copy path: TPM mTLS when configured, a
	// server-verification transport for private-CA/self-signed registries, else
	// the library default. oras dials the token endpoint through client.Client
	// too, so the client certificate is presented during token acquisition.
	rt, err := xport.Transport(c)
	if err != nil {
		return nil, err
	}
	if rt != nil {
		client.Client = &http.Client{Transport: rt}
	}
	if c.Insecure {
		// Plain-HTTP registries; self-signed TLS parity is handled by storeTransport.
		r.PlainHTTP = true
	}
	r.Client = client
	return r, nil
}

// copyReferrers copies every referrer artifact of subject (e.g. notation
// signatures) from the source repo into the cache repo, so a downstream
// verifier can re-verify the image against the cache. The subject manifest must
// already exist in the destination (Commit runs first). oras handles both the
// referrers API and the fallback-tag scheme, whichever the registry supports.
// Returns the number of referrers copied.
// countReferrers reports how many referrer artifacts a store holds for a subject.
// Used to tell "this store has the image" from "this store has the image AND the
// signatures over it", which are different answers when a job propagates them.
func countReferrers(ctx context.Context, store config.StoreConfig, subject name.Digest) (int, error) {
	repo, err := orasRepo(store, subject.Context())
	if err != nil {
		return 0, z.Err(err, "repo")
	}
	desc, err := repo.Resolve(ctx, subject.DigestStr())
	if err != nil {
		return 0, z.Err(err, "resolve subject %s", subject.DigestStr())
	}
	n := 0
	if err := repo.Referrers(ctx, desc, "", func(ds []ocispec.Descriptor) error {
		n += len(ds)
		return nil
	}); err != nil {
		return 0, z.Err(err, "list referrers")
	}
	return n, nil
}

func copyReferrers(ctx context.Context, source, target config.StoreConfig, src name.Reference, subject v1.Hash, dst name.Repository) (int, error) {
	src_repo, err := orasRepo(source, src.Context())
	if err != nil {
		return 0, z.Err(err, "source repo")
	}
	dst_repo, err := orasRepo(target, dst)
	if err != nil {
		return 0, z.Err(err, "destination repo")
	}
	desc, err := src_repo.Resolve(ctx, subject.String())
	if err != nil {
		return 0, z.Err(err, "resolve subject %s", subject)
	}
	var refs []ocispec.Descriptor
	err = src_repo.Referrers(ctx, desc, "", func(ds []ocispec.Descriptor) error {
		refs = append(refs, ds...)
		return nil
	})
	if err != nil {
		return 0, z.Err(err, "list referrers")
	}
	for _, d := range refs {
		if err := oras.CopyGraph(ctx, src_repo, dst_repo, d, oras.DefaultCopyGraphOptions); err != nil {
			return 0, z.Err(err, "copy referrer %s", d.Digest)
		}
	}
	return len(refs), nil
}
