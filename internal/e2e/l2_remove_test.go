//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// StoreService.Remove on a registry store, against a real registry: gantry's
// answer matches what the registry did. A manifest reported deleted no longer
// resolves, and one reported refused still does.
//
// Whether a registry deletes at all is its own configuration: distribution 2.x
// ships with `storage.delete.enabled` off and 3.x with it on, and cr deletes.
// cr is the registry this exists for, so there the delete has to succeed.
func TestL2RemoveFromARegistryStore(t *testing.T) {
	h := newL2Harness(t)
	dg := seedImage(t, h.cache, "dist/app", "1").String()

	_, err := h.client.Store().Remove(context.Background(), pb.StoreRemoveRequest_builder{
		Store: pb.StoreByName("cache"),
		Ref:   proto.String("dist/app@" + dg),
	}.Build())
	_, lookup := digestByRef(t, h.cache, "dist/app", dg)
	held := lookup == nil
	switch {
	case err == nil && held:
		t.Error("the manifest was reported deleted and still resolves")
	case err != nil && status.Code(err) != codes.FailedPrecondition:
		t.Errorf("err = %v, want success or FAILED_PRECONDITION", err)
	case err != nil && !held:
		t.Errorf("the manifest is gone, yet the delete was reported refused: %v", err)
	}
	if isCR(registryImage()) && err != nil {
		t.Errorf("cr refused the delete: %v", err)
	}
	t.Logf("%s: manifest delete err=%v, manifest held afterwards=%v", registryImage(), err, held)
}

// A tag delete is optional in the distribution spec. Whatever the registry
// does with one, gantry's answer has to match it: a tag reported removed no
// longer resolves, and one reported refused still does.
func TestL2RemoveATagReportsWhatTheRegistryDid(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.cache, "dist/app", "1")

	_, err := h.client.Store().Remove(context.Background(), pb.StoreRemoveRequest_builder{
		Store: pb.StoreByName("cache"),
		Ref:   proto.String("dist/app:1"),
	}.Build())
	held := hasTag(t, h.cache, "dist/app", "1")
	switch {
	case err == nil && held:
		t.Error("the tag was reported removed and still resolves")
	case err != nil && status.Code(err) != codes.FailedPrecondition:
		t.Errorf("err = %v, want success or FAILED_PRECONDITION", err)
	case err != nil && !held:
		t.Errorf("the tag is gone, yet the delete was reported refused: %v", err)
	}
	t.Logf("%s: tag delete err=%v, tag held afterwards=%v", registryImage(), err, held)
}
