// Package tpm provides a crypto.Signer backed by a key held in a TPM, addressed
// by its persistent handle. The private key never leaves the device: signing is
// performed by the TPM, so the signer can back a TLS client certificate for mTLS
// without the key material ever being present in process memory.
package tpm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/asn1"
	"fmt"
	"io"
	"math/big"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// Signer implements crypto.Signer backed by a TPM persistent key. It is safe for
// concurrent use when its transport serializes commands (see Locking).
type Signer struct {
	rw     transport.TPM
	handle tpm2.TPMHandle
	name   tpm2.TPM2BName
	pub    crypto.PublicKey
}

// NewSigner reads the public area of the key at handle and returns a Signer over
// it. handle must reference a persistent signing key already provisioned in the
// TPM (gantry does not create keys).
func NewSigner(rw transport.TPM, handle tpm2.TPMHandle) (*Signer, error) {
	resp, err := tpm2.ReadPublic{
		ObjectHandle: handle,
	}.Execute(rw)
	if err != nil {
		return nil, fmt.Errorf("ReadPublic 0x%x: %w", uint32(handle), err)
	}

	pub, err := resp.OutPublic.Contents()
	if err != nil {
		return nil, fmt.Errorf("parse public area: %w", err)
	}

	cryptoPub, err := pubToGo(pub)
	if err != nil {
		return nil, err
	}

	return &Signer{rw: rw, handle: handle, name: resp.Name, pub: cryptoPub}, nil
}

func (s *Signer) Public() crypto.PublicKey { return s.pub }

// Sign runs TPM2_Sign over digest and returns an ASN.1 DER ECDSA signature — the
// encoding crypto/tls expects for an ECDSA client certificate. rand is ignored;
// the TPM is the source of the nonce.
func (s *Signer) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	hashAlg, err := hashAlgFromOpts(opts)
	if err != nil {
		return nil, err
	}

	resp, err := tpm2.Sign{
		KeyHandle: tpm2.AuthHandle{
			Handle: s.handle,
			Name:   s.name,
			Auth:   tpm2.PasswordAuth(nil),
		},
		Digest: tpm2.TPM2BDigest{Buffer: digest},
		InScheme: tpm2.TPMTSigScheme{
			Scheme: tpm2.TPMAlgECDSA,
			Details: tpm2.NewTPMUSigScheme(
				tpm2.TPMAlgECDSA,
				&tpm2.TPMSSchemeHash{HashAlg: hashAlg},
			),
		},
		Validation: tpm2.TPMTTKHashCheck{Tag: tpm2.TPMSTHashCheck},
	}.Execute(s.rw)
	if err != nil {
		return nil, fmt.Errorf("TPM2_Sign: %w", err)
	}

	ecSig, err := resp.Signature.Signature.ECDSA()
	if err != nil {
		return nil, fmt.Errorf("decode ECDSA signature: %w", err)
	}

	return asn1.Marshal(struct{ R, S *big.Int }{
		R: new(big.Int).SetBytes(ecSig.SignatureR.Buffer),
		S: new(big.Int).SetBytes(ecSig.SignatureS.Buffer),
	})
}

func pubToGo(pub *tpm2.TPMTPublic) (crypto.PublicKey, error) {
	switch pub.Type {
	case tpm2.TPMAlgECC:
		detail, err := pub.Parameters.ECCDetail()
		if err != nil {
			return nil, err
		}
		unique, err := pub.Unique.ECC()
		if err != nil {
			return nil, err
		}
		curve, err := eccCurve(detail.CurveID)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(unique.X.Buffer),
			Y:     new(big.Int).SetBytes(unique.Y.Buffer),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported TPM key type: %v (only ECC is supported)", pub.Type)
	}
}

func eccCurve(id tpm2.TPMECCCurve) (elliptic.Curve, error) {
	switch id {
	case tpm2.TPMECCNistP256:
		return elliptic.P256(), nil
	case tpm2.TPMECCNistP384:
		return elliptic.P384(), nil
	case tpm2.TPMECCNistP521:
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported ECC curve: %v", id)
	}
}

func hashAlgFromOpts(opts crypto.SignerOpts) (tpm2.TPMIAlgHash, error) {
	switch opts.HashFunc() {
	case crypto.SHA256:
		return tpm2.TPMAlgSHA256, nil
	case crypto.SHA384:
		return tpm2.TPMAlgSHA384, nil
	case crypto.SHA512:
		return tpm2.TPMAlgSHA512, nil
	default:
		return 0, fmt.Errorf("unsupported hash: %v", opts.HashFunc())
	}
}
