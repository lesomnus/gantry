package config

import "testing"

func TestTPMHandleValue(t *testing.T) {
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
		got, err := StoreConfig{TPMHandle: c.in}.TPMHandleValue()
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("TPMHandleValue(%q) = %#x, %v; want %#x, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("TPMHandleValue(%q) expected error", c.in)
		}
	}
}

func TestValidateTPM(t *testing.T) {
	t.Run("none is ok", func(t *testing.T) {
		if err := (StoreConfig{Kind: "oci"}).validateTPM(); err != nil {
			t.Errorf("no TPM config should validate: %v", err)
		}
	})
	t.Run("handle without cert is rejected", func(t *testing.T) {
		if err := (StoreConfig{TPMHandle: "0x81000001"}).validateTPM(); err == nil {
			t.Error("expected tpm_cert-required error")
		}
	})
	t.Run("cert without handle is rejected", func(t *testing.T) {
		if err := (StoreConfig{TPMCert: "/x.crt"}).validateTPM(); err == nil {
			t.Error("expected tpm_handle-required error")
		}
	})
	t.Run("bad handle is rejected", func(t *testing.T) {
		if err := (StoreConfig{TPMHandle: "nope", TPMCert: "/x.crt"}).validateTPM(); err == nil {
			t.Error("expected bad-handle error")
		}
	})
	t.Run("complete is ok", func(t *testing.T) {
		if err := (StoreConfig{TPMHandle: "0x81000001", TPMCert: "/x.crt"}).validateTPM(); err != nil {
			t.Errorf("complete TPM config should validate: %v", err)
		}
	})
	t.Run("ca_cert alone is ok (server verification only)", func(t *testing.T) {
		if err := (StoreConfig{CACert: "/ca.crt"}).validateTPM(); err != nil {
			t.Errorf("ca_cert without TPM should validate: %v", err)
		}
	})
	t.Run("insecure with TPM is rejected", func(t *testing.T) {
		s := StoreConfig{TPMHandle: "0x81000001", TPMCert: "/x.crt", Insecure: true}
		if err := s.validateTPM(); err == nil {
			t.Error("expected insecure+TPM to be rejected")
		}
	})
}

func TestEvaluateTPMDefaultsDevice(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"upstream": {Kind: "oci", TPMHandle: "0x81000001", TPMCert: "/etc/gantry/device.crt"},
	}}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	s := c.Stores["upstream"]
	if s.TPMDevice != "/dev/tpmrm0" {
		t.Errorf("tpm device default = %q; want /dev/tpmrm0", s.TPMDevice)
	}
	if !s.HasTPM() {
		t.Error("HasTPM() should be true")
	}
}

func TestEvaluateRejectsBadTPMHandle(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"upstream": {Kind: "oci", TPMHandle: "not-hex", TPMCert: "/x.crt"},
	}}
	if err := c.Evaluate(); err == nil {
		t.Error("expected evaluate to reject an invalid tpm_handle")
	}
}
