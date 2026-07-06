package tpm

import (
	"fmt"
	"io"
	"sync"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

// DefaultDevice is the Linux TPM resource-manager device used when no device is
// configured. The resource manager (tpmrm0) multiplexes access across clients,
// unlike the raw tpm0 device.
const DefaultDevice = "/dev/tpmrm0"

// locking serializes access to a TPM transport. A single TPM connection is one
// file descriptor with no internal locking (Send does Write then Read), so
// concurrent callers — e.g. parallel TLS handshakes signing with the same key —
// would interleave command/response bytes and corrupt each other.
type locking struct {
	mu    sync.Mutex
	inner transport.TPM
}

// Locking wraps a TPM transport so every command is serialized. Wrap the shared
// transport once at open time and hand the result to all users of the device.
func Locking(inner transport.TPM) transport.TPM {
	return &locking{inner: inner}
}

func (l *locking) Send(input []byte) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.Send(input)
}

// OpenSigner opens the TPM device and returns a Signer over the persistent key
// at handle. The transport is serialized, so the Signer is safe for concurrent
// use (e.g. parallel TLS handshakes). Close the returned io.Closer to release
// the device. An empty device defaults to DefaultDevice.
func OpenSigner(device string, handle uint32) (*Signer, io.Closer, error) {
	if device == "" {
		device = DefaultDevice
	}
	rw, err := linuxtpm.Open(device)
	if err != nil {
		return nil, nil, fmt.Errorf("open TPM %q: %w", device, err)
	}
	s, err := NewSigner(Locking(rw), tpm2.TPMHandle(handle))
	if err != nil {
		_ = rw.Close()
		return nil, nil, fmt.Errorf("load TPM key 0x%x: %w", handle, err)
	}
	return s, rw, nil
}
