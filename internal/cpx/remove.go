package cpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/z"
)

// ErrDeleteUnsupported is a registry that answered a delete by saying it does
// not do deletes — distribution without `storage.delete.enabled`, or one that
// deletes manifests but not tags. It is a property of the registry, not a
// failure to reach it, so it is reported as such rather than retried.
var ErrDeleteUnsupported = errors.New("the registry does not allow this delete")

// ErrDeleteDenied is a registry that refused this credential the delete. Unlike
// a probe, where a 403 is how some registries say "no such repository", a
// refused DELETE is a refusal: saying the image is not there would be a lie.
var ErrDeleteDenied = errors.New("the registry refused the delete")

// ErrInvalidReference is a ref that does not name anything a registry could
// hold — the caller's mistake, not the registry's answer.
var ErrInvalidReference = errors.New("invalid reference")

// RegistryRemoveResult is what a registry delete took away, in the shape an
// engine's removal reports: a tag that stops resolving, or a manifest that is
// gone. What that frees on disk is the registry's own garbage collection's to
// decide, which is why neither is called "deleted bytes".
type RegistryRemoveResult struct {
	Untagged []string // tag refs that no longer resolve
	Deleted  []string // manifest digests removed
}

// RemoveFromRegistry deletes what ref names from a registry store: the manifest
// when ref is a digest, the tag when ref is a tag. Nothing is resolved first.
// Deleting by tag and deleting the manifest it points at are different things —
// a digest delete takes every tag on that manifest with it in most registries —
// and the caller says which one it means.
//
// ref is a repository path in the store, "team/app@sha256:…" or "team/app:1",
// the same form a job names its source by. A leading "<host>/" matching the
// store's own host is accepted and dropped, since that is how the image is
// spelled on a node that pulled it.
func RemoveFromRegistry(ctx context.Context, store config.StoreConfig, ref string) (RegistryRemoveResult, error) {
	if !store.IsRegistry() {
		return RegistryRemoveResult{}, fmt.Errorf("store %q is kind %q, not a registry", store.Name, store.Kind)
	}
	ref = strings.TrimPrefix(ref, store.Host+"/")
	opts := []name.Option{name.StrictValidation}
	if store.Insecure {
		opts = append(opts, name.Insecure)
	}
	r, err := name.ParseReference(store.Host+"/"+ref, opts...)
	if err != nil {
		return RegistryRemoveResult{}, fmt.Errorf("%w: %q in store %q: %v", ErrInvalidReference, ref, store.Name, err)
	}
	rt, err := xport.Transport(store)
	if err != nil {
		return RegistryRemoveResult{}, z.Err(err, "store %q outbound transport", store.Name)
	}
	if err := remote.Delete(r, baseOpts(ctx, registryAuth(store), rt)...); err != nil {
		return RegistryRemoveResult{}, deleteErr(err, r, store)
	}
	if dg, ok := r.(name.Digest); ok {
		return RegistryRemoveResult{Deleted: []string{dg.DigestStr()}}, nil
	}
	return RegistryRemoveResult{Untagged: []string{r.Name()}}, nil
}

// deleteErr sorts a registry's answer to a DELETE into what the caller can act
// on: not there, not allowed here, not allowed to you, or could not ask.
func deleteErr(err error, r name.Reference, store config.StoreConfig) error {
	var te *transport.Error
	if !errors.As(err, &te) {
		return z.Err(err, "delete %q at %q", r.Name(), store.Name)
	}
	switch {
	case te.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %q at %q", ErrNoSuchImage, r.Name(), store.Name)
	case te.StatusCode == http.StatusMethodNotAllowed || hasCode(te, transport.UnsupportedErrorCode):
		return fmt.Errorf("%w: %q at %q: %v", ErrDeleteUnsupported, r.Name(), store.Name, err)
	case te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden || hasCode(te, transport.DeniedErrorCode):
		return fmt.Errorf("%w: %q at %q: %v", ErrDeleteDenied, r.Name(), store.Name, err)
	}
	return z.Err(err, "delete %q at %q", r.Name(), store.Name)
}

func hasCode(te *transport.Error, code transport.ErrorCode) bool {
	for _, d := range te.Errors {
		if d.Code == code {
			return true
		}
	}
	return false
}
