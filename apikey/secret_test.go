package apikey

import (
	"strings"
	"testing"
)

func TestDigestStableAndPepperSensitive(t *testing.T) {
	a := Digest("sk-secret", []byte("pepper-a"))
	b := Digest("sk-secret", []byte("pepper-a"))
	c := Digest("sk-secret", []byte("pepper-b"))
	if a != b {
		t.Fatalf("same inputs must hash equal")
	}
	if a == c {
		t.Fatalf("different pepper must not hash equal")
	}
	if len(a) != 64 {
		t.Fatalf("expected hex sha256 length 64, got %d", len(a))
	}
}

func TestGenerateSecretFormat(t *testing.T) {
	s := GenerateSecret()
	if !strings.HasPrefix(s, "sk-") {
		t.Fatalf("prefix: %q", s)
	}
	if len(s) != 3+64 {
		t.Fatalf("length: %d", len(s))
	}
	pt := GeneratePortalToken()
	if !strings.HasPrefix(pt, "pt-") {
		t.Fatalf("portal token prefix: %q", pt)
	}
}

func TestMaskNeverReturnsShortPlaintext(t *testing.T) {
	got := MaskSecret("sk-ab")
	if got == "sk-ab" {
		t.Fatalf("short secrets must still be masked, got %q", got)
	}
	if !strings.Contains(got, "••••••••") {
		t.Fatalf("expected bullets, got %q", got)
	}
}
