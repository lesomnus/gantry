// Command e2e-seed populates a registry with synthetic images (and, optionally,
// notation signatures) for the E2E environment — egress-free, so a run needs no
// Docker Hub pulls. It reuses gantry's existing dependencies (go-containerregistry,
// notation-go, oras); there are no new modules.
//
//	e2e-seed --to 127.0.0.1:5000 --repo lib/app --tag 1 --insecure
//	e2e-seed --to 127.0.0.1:5000 --repo lib/app --tag 1 --insecure --sign --ca-out ca.crt
//	e2e-seed --to 127.0.0.1:5000 --repo lib/multi --tag 1 --insecure --platforms linux/amd64,linux/arm64
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/notaryproject/notation-core-go/signature/jws"
	"github.com/notaryproject/notation-core-go/testhelper"
	"github.com/notaryproject/notation-go"
	notationregistry "github.com/notaryproject/notation-go/registry"
	"github.com/notaryproject/notation-go/signer"
	orasremote "oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-seed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		to        = flag.String("to", "", "target registry host:port (required)")
		repo      = flag.String("repo", "lib/app", "repository")
		tag       = flag.String("tag", "1", "tag")
		insecure  = flag.Bool("insecure", false, "use plain HTTP")
		sign      = flag.Bool("sign", false, "notation-sign the image and push the signature as a referrer")
		caOut     = flag.String("ca-out", "", "write the signing CA certificate here (for a gantry trust store)")
		platforms = flag.String("platforms", "", "comma-separated os/arch list; makes a multi-platform index")
	)
	flag.Parse()
	if *to == "" {
		flag.Usage()
		return fmt.Errorf("--to is required")
	}

	fullRef := fmt.Sprintf("%s/%s:%s", *to, *repo, *tag)
	ref, err := parseRef(fullRef, *insecure)
	if err != nil {
		return err
	}

	dg, err := push(ref, *platforms)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}

	if *sign {
		if err := signImage(fullRef, *insecure); err != nil {
			return fmt.Errorf("sign: %w", err)
		}
		if *caOut != "" {
			if err := writeCA(*caOut); err != nil {
				return fmt.Errorf("write ca: %w", err)
			}
		}
	}
	fmt.Printf("%s@%s\n", fullRef, dg)
	return nil
}

func parseRef(ref string, insecure bool) (name.Reference, error) {
	if insecure {
		return name.ParseReference(ref, name.Insecure)
	}
	return name.ParseReference(ref)
}

func push(ref name.Reference, platforms string) (v1.Hash, error) {
	if platforms == "" {
		img, err := random.Image(2048, 3)
		if err != nil {
			return v1.Hash{}, err
		}
		if err := remote.Write(ref, img); err != nil {
			return v1.Hash{}, err
		}
		return img.Digest()
	}
	idx := v1.ImageIndex(empty.Index)
	var adds []mutate.IndexAddendum
	for _, p := range strings.Split(platforms, ",") {
		img, err := random.Image(1024, 2)
		if err != nil {
			return v1.Hash{}, err
		}
		os, arch, _ := strings.Cut(strings.TrimSpace(p), "/")
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}},
		})
	}
	idx = mutate.AppendManifests(idx, adds...)
	if err := remote.WriteIndex(ref, idx); err != nil {
		return v1.Hash{}, err
	}
	return idx.Digest()
}

// signImage signs fullRef with a fixed test identity and pushes the notation
// signature as a referrer. The matching CA is testhelper's RSA root (see
// writeCA) — a fixed E2E signing identity, not a production one.
func signImage(fullRef string, insecure bool) error {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	s, err := signer.New(leaf.PrivateKey, []*x509.Certificate{leaf.Cert, root.Cert})
	if err != nil {
		return err
	}
	ref, err := parseRef(fullRef, insecure)
	if err != nil {
		return err
	}
	r, err := orasremote.NewRepository(ref.Context().Name())
	if err != nil {
		return err
	}
	r.PlainHTTP = insecure
	r.Client = &auth.Client{Cache: auth.NewCache()}
	_, err = notation.Sign(context.Background(), s, notationregistry.NewRepository(r), notation.SignOptions{
		SignerSignOptions: notation.SignerSignOptions{SignatureMediaType: jws.MediaTypeEnvelope},
		ArtifactReference: fullRef,
	})
	return err
}

func writeCA(path string) error {
	root := testhelper.GetRSARootCertificate()
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw})
	return os.WriteFile(path, b, 0o644)
}
