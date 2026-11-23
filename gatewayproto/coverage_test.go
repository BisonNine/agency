package gatewayproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestVerifyDeviceSignature_UndecodableSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyDeviceSignature(b64url(pub), "p", "not-base64-!!") {
		t.Fatal("expected false for an undecodable signature")
	}
}

func TestNewSetupPayload_Defaults(t *testing.T) {
	p := NewSetupPayload(nil, 18790, "tok", "")
	if p.IPs == nil || len(p.IPs) != 0 {
		t.Errorf("IPs = %#v, want an empty non-nil list", p.IPs)
	}
	if p.Protocol != SetupProtocolWS || p.Type != SetupPayloadType || p.Version != SetupPayloadVersion {
		t.Errorf("payload = %+v", p)
	}
	s, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if want := `"ips":[]`; !strings.Contains(s, want) {
		t.Errorf("encoded %s lacks %s", s, want)
	}
}
