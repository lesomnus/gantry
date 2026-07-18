package e2e

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"github.com/notaryproject/notation-core-go/signature/jws"
	"github.com/notaryproject/notation-core-go/testhelper"
	"github.com/notaryproject/notation-go"
	notationregistry "github.com/notaryproject/notation-go/registry"
	"github.com/notaryproject/notation-go/signer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	orasremote "oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func writeTrustStore(t *testing.T, ca *x509.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	if err := os.WriteFile(filepath.Join(dir, "root.crt"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// signRef signs fullRef in-process with a fresh leaf chaining to root, pushing
// the notation signature as a referrer to the (in-memory) registry.
func signRef(t *testing.T, fullRef string, root, leaf testhelper.RSACertTuple) {
	t.Helper()
	s, err := signer.New(leaf.PrivateKey, []*x509.Certificate{leaf.Cert, root.Cert})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(fullRef, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	r, err := orasremote.NewRepository(ref.Context().Name())
	if err != nil {
		t.Fatal(err)
	}
	r.PlainHTTP = true
	r.Client = &auth.Client{Cache: auth.NewCache()}
	if _, err := notation.Sign(context.Background(), s, notationregistry.NewRepository(r), notation.SignOptions{
		SignerSignOptions: notation.SignerSignOptions{SignatureMediaType: jws.MediaTypeEnvelope},
		ArtifactReference: fullRef,
	}); err != nil {
		t.Fatalf("notation sign: %v", err)
	}
}

// Feature 8: signature verification (in-process notation). Under `require` a
// signed source is admitted (and its digest pinned) while an unsigned one is
// rejected; copy_referrers carries the signature into the cache.
func TestVerification(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trust := writeTrustStore(t, root.Cert)

	h := newHarness(t, withVerify(config.VerifyConfig{
		Mode: config.VerifyRequire, Provider: "notation", TrustStore: trust,
		Level: "permissive", Timeout: config.Duration(20 * time.Second),
	}))

	signedDigest := seedImage(t, h.remote, "lib/signed", "1")
	signRef(t, h.remote+"/lib/signed:1", root, leaf)
	seedImage(t, h.remote, "lib/unsigned", "1")

	vc := pb.NewVerifyServiceClient(h.conn)

	// A signed source verifies and reports the pinned digest.
	rep, err := vc.Check(context.Background(), pb.VerifyCheckRequest_builder{
		Ref: proto.String("lib/signed:1"), Source: pb.StoreByName("remote"),
	}.Build())
	if err != nil {
		t.Fatalf("check signed: %v", err)
	}
	if !rep.GetVerified() {
		t.Error("signed image did not verify")
	}
	if rep.GetDigest() != signedDigest.String() {
		t.Errorf("pinned digest = %s, want %s", rep.GetDigest(), signedDigest)
	}

	// An unsigned source is rejected under require mode.
	if _, err := vc.Check(context.Background(), pb.VerifyCheckRequest_builder{
		Ref: proto.String("lib/unsigned:1"), Source: pb.StoreByName("remote"),
	}.Build()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unsigned check error = %v, want FailedPrecondition", err)
	}

	// A verified copy with copy_referrers carries the signature into the cache,
	// so it re-verifies there.
	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref: "lib/signed:1", Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
		CopyReferrers: proto.Bool(true),
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("verified copy state=%v error=%q", job.GetState(), job.GetError())
	}
	rep, err = vc.Check(context.Background(), pb.VerifyCheckRequest_builder{
		Ref: proto.String("lib/signed:1"), Source: pb.StoreByName("cache"),
	}.Build())
	if err != nil || !rep.GetVerified() {
		t.Errorf("signature did not travel into the cache: verified=%v err=%v", rep.GetVerified(), err)
	}
}
