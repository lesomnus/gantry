package down

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// The thin archive names already-present content: the anchor bytes as the only
// blob, and one index entry per requested name.
func TestAnchorArchive(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	sum := sha256.Sum256(raw)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	names := []string{"cr.example.com/team/app@" + dg, "other.example.com/app@" + dg}

	b, err := anchorArchive(names, &AnchorBlob{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Digest:    dg,
		Bytes:     raw,
	})
	if err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = data
	}

	if string(files["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Errorf("oci-layout = %s", files["oci-layout"])
	}
	if got := files["blobs/sha256/"+strings.TrimPrefix(dg, "sha256:")]; !bytes.Equal(got, raw) {
		t.Errorf("anchor blob does not round-trip")
	}

	var idx struct {
		SchemaVersion int `json:"schemaVersion"`
		Manifests     []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	if idx.SchemaVersion != 2 || len(idx.Manifests) != 2 {
		t.Fatalf("index = %s", files["index.json"])
	}
	for i, m := range idx.Manifests {
		if m.Digest != dg || m.Size != int64(len(raw)) || m.MediaType != "application/vnd.oci.image.index.v1+json" {
			t.Errorf("manifest[%d] = %+v", i, m)
		}
		if m.Annotations["io.containerd.image.name"] != names[i] {
			t.Errorf("manifest[%d] name = %q, want %q", i, m.Annotations["io.containerd.image.name"], names[i])
		}
	}

	// A digest scheme other than sha256 cannot be laid out.
	if _, err := anchorArchive(names, &AnchorBlob{MediaType: "m", Digest: "sha512:00", Bytes: raw}); err == nil {
		t.Error("sha512 anchor must be rejected")
	}
}
