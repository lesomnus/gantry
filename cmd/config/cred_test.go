package config

import "testing"

func TestCredHandleValue(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"0x81000001", 0x81000001, true},
		{"0x81686479", 0x81686479, true}, // top-bit-set persistent handle
		{"2164260865", 0x81000001, true}, // decimal
		{"", 0, false},
		{"nope", 0, false},
		{"0x1_0000_0000", 0, false}, // > 32 bits
	}
	for _, c := range cases {
		got, err := CredConfig{Handle: c.in}.HandleValue()
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("HandleValue(%q) = %#x, %v; want %#x, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("HandleValue(%q) expected error", c.in)
		}
	}
}

func TestValidateCred(t *testing.T) {
	validate := func(cred *CredConfig, insecure bool) error {
		return StoreConfig{Kind: "oci", Cred: cred, Insecure: insecure}.validateCred()
	}

	t.Run("no cred is ok", func(t *testing.T) {
		if err := validate(nil, false); err != nil {
			t.Errorf("no cred should validate: %v", err)
		}
	})
	t.Run("ca_cert alone is ok (server verification only)", func(t *testing.T) {
		if err := (StoreConfig{CACert: "/ca.crt"}).validateCred(); err != nil {
			t.Errorf("ca_cert without cred should validate: %v", err)
		}
	})
	t.Run("unknown kind is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Kind: "vault", Cert: "/x.crt"}, false); err == nil {
			t.Error("expected unknown-kind error")
		}
	})
	t.Run("empty kind is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Cert: "/x.crt", Key: "/x.key"}, false); err == nil {
			t.Error("expected kind-required error")
		}
	})
	t.Run("missing cert is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Kind: "tpm", Handle: "0x81000001"}, false); err == nil {
			t.Error("expected cred.cert-required error")
		}
	})
	t.Run("insecure with cred is rejected", func(t *testing.T) {
		cred := &CredConfig{Kind: "file", Cert: "/x.crt", Key: "/x.key"}
		if err := validate(cred, true); err == nil {
			t.Error("expected insecure+cred to be rejected")
		}
	})

	t.Run("tpm: complete is ok", func(t *testing.T) {
		cred := &CredConfig{Kind: "tpm", Handle: "0x81000001", Cert: "/x.crt"}
		if err := validate(cred, false); err != nil {
			t.Errorf("complete tpm cred should validate: %v", err)
		}
		if !cred.IsTPM() {
			t.Error("IsTPM() should be true")
		}
	})
	t.Run("tpm: missing handle is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Kind: "tpm", Cert: "/x.crt"}, false); err == nil {
			t.Error("expected cred.handle-required error")
		}
	})
	t.Run("tpm: bad handle is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Kind: "tpm", Handle: "nope", Cert: "/x.crt"}, false); err == nil {
			t.Error("expected bad-handle error")
		}
	})
	t.Run("tpm: key is rejected", func(t *testing.T) {
		cred := &CredConfig{Kind: "tpm", Handle: "0x81000001", Cert: "/x.crt", Key: "/x.key"}
		if err := validate(cred, false); err == nil {
			t.Error("expected cred.key to be rejected for kind tpm")
		}
	})

	t.Run("file: complete is ok", func(t *testing.T) {
		cred := &CredConfig{Kind: "file", Cert: "/x.crt", Key: "/x.key"}
		if err := validate(cred, false); err != nil {
			t.Errorf("complete file cred should validate: %v", err)
		}
		if cred.IsTPM() {
			t.Error("IsTPM() should be false")
		}
	})
	t.Run("file: missing key is rejected", func(t *testing.T) {
		if err := validate(&CredConfig{Kind: "file", Cert: "/x.crt"}, false); err == nil {
			t.Error("expected cred.key-required error")
		}
	})
	t.Run("file: handle or device is rejected", func(t *testing.T) {
		cred := &CredConfig{Kind: "file", Cert: "/x.crt", Key: "/x.key", Handle: "0x81000001"}
		if err := validate(cred, false); err == nil {
			t.Error("expected cred.handle to be rejected for kind file")
		}
		cred = &CredConfig{Kind: "file", Cert: "/x.crt", Key: "/x.key", Device: "/dev/tpmrm0"}
		if err := validate(cred, false); err == nil {
			t.Error("expected cred.device to be rejected for kind file")
		}
	})
}

func TestEvaluateTPMDefaultsDevice(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"upstream": {Kind: "oci", Cred: &CredConfig{Kind: "tpm", Handle: "0x81000001", Cert: "/etc/gantry/device.crt"}},
	}}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	s := c.Stores["upstream"]
	if s.Cred.Device != "/dev/tpmrm0" {
		t.Errorf("cred.device default = %q; want /dev/tpmrm0", s.Cred.Device)
	}
	if !s.Cred.IsTPM() {
		t.Error("IsTPM() should be true")
	}
}

func TestEvaluateFileCredKeepsDeviceEmpty(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"upstream": {Kind: "oci", Cred: &CredConfig{Kind: "file", Cert: "/x.crt", Key: "/x.key"}},
	}}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d := c.Stores["upstream"].Cred.Device; d != "" {
		t.Errorf("file cred should not get a TPM device default, got %q", d)
	}
}

func TestEvaluateRejectsBadTPMHandle(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"upstream": {Kind: "oci", Cred: &CredConfig{Kind: "tpm", Handle: "not-hex", Cert: "/x.crt"}},
	}}
	if err := c.Evaluate(); err == nil {
		t.Error("expected evaluate to reject an invalid cred.handle")
	}
}
