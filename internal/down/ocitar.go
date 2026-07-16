package down

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// anchorArchive builds a thin OCI layout archive that names already-present
// content: the anchor manifest/index as its blob, and one top-level index entry
// per requested name (annotation io.containerd.image.name — the containerd
// image store registers each verbatim). No config or layer blobs travel; the
// pull that preceded the load placed them.
func anchorArchive(names []string, anchor *AnchorBlob) ([]byte, error) {
	hex, ok := strings.CutPrefix(anchor.Digest, "sha256:")
	if !ok || hex == "" {
		return nil, fmt.Errorf("unsupported anchor digest %q", anchor.Digest)
	}
	mt := anchor.MediaType
	if mt == "" {
		return nil, fmt.Errorf("anchor media type unknown")
	}

	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	entries := make([]descriptor, 0, len(names))
	for _, n := range names {
		entries = append(entries, descriptor{
			MediaType:   mt,
			Digest:      anchor.Digest,
			Size:        int64(len(anchor.Bytes)),
			Annotations: map[string]string{"io.containerd.image.name": n},
		})
	}
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": entries})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + hex, anchor.Bytes},
	} {
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
