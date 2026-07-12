package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func setupResponsesStore(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

func newStoredResponse(id string) *ResponsesObject {
	return &ResponsesObject{
		ID:       id,
		Object:   "response",
		Status:   "completed",
		Model:    "claude-sonnet-4.5",
		StoredAt: time.Now().Unix(),
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "hi"}},
		}},
	}
}

func TestResponseOwnerSamePrincipalAllowed(t *testing.T) {
	setupResponsesStore(t)
	if err := saveResponse(newStoredResponse("resp_owned"), "key-A", "key-A"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadResponseForOwner("resp_owned", "key-A", true)
	if err != nil {
		t.Fatalf("owner should load its own response: %v", err)
	}
	if got.ID != "resp_owned" {
		t.Fatalf("wrong response: %+v", got)
	}
}

func TestResponseOwnerDifferentPrincipalDenied(t *testing.T) {
	setupResponsesStore(t)
	if err := saveResponse(newStoredResponse("resp_owned"), "key-A", "key-A"); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, err := loadResponseForOwner("resp_owned", "key-B", true)
	if err == nil {
		t.Fatal("a different principal must not read another key's response")
	}
	if err != errResponseNotFound {
		t.Fatalf("expected generic not-found (no existence leak), got %v", err)
	}
}

func TestResponseMissingAndCrossOwnerReturnSameError(t *testing.T) {
	setupResponsesStore(t)
	if err := saveResponse(newStoredResponse("resp_owned"), "key-A", "key-A"); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, missErr := loadResponseForOwner("resp_missing", "key-B", true)
	_, crossErr := loadResponseForOwner("resp_owned", "key-B", true)
	if missErr != errResponseNotFound || crossErr != errResponseNotFound {
		t.Fatalf("missing (%v) and cross-owner (%v) must be indistinguishable", missErr, crossErr)
	}
}

func TestResponseKeyRotationPreservesOwnershipViaStableID(t *testing.T) {
	setupResponsesStore(t)
	// Ownership is keyed by the stable ApiKeyEntry.ID, not the raw key value.
	// Rotating the raw key in place keeps the ID, so the conversation survives.
	if err := saveResponse(newStoredResponse("resp_rot"), "stable-id", "stable-id"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadResponseForOwner("resp_rot", "stable-id", true)
	if err != nil || got == nil {
		t.Fatalf("rotation-in-place (same ID) must keep access: %v", err)
	}
}

func TestResponseLegacyOwnerEmptyDeniedUnderAuth(t *testing.T) {
	setupResponsesStore(t)
	// Simulate a legacy doc written before ownership existed: empty owner.
	if err := saveResponse(newStoredResponse("resp_legacy"), "", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Auth enabled: legacy orphan must NOT be adopted by whoever asks first.
	if _, err := loadResponseForOwner("resp_legacy", "key-A", true); err != errResponseNotFound {
		t.Fatalf("legacy owner-empty doc must be denied under auth, got %v", err)
	}
}

func TestResponseLegacyOwnerEmptyAllowedWhenAuthDisabled(t *testing.T) {
	setupResponsesStore(t)
	if err := saveResponse(newStoredResponse("resp_legacy"), "", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Auth disabled: single-user scope, legacy doc is accessible.
	if _, err := loadResponseForOwner("resp_legacy", anonymousOwner, false); err != nil {
		t.Fatalf("legacy doc should be readable in single-user mode: %v", err)
	}
}

func TestResponseAnonymousScopeSharedWhenAuthDisabled(t *testing.T) {
	setupResponsesStore(t)
	if err := saveResponse(newStoredResponse("resp_anon"), anonymousOwner, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := loadResponseForOwner("resp_anon", anonymousOwner, false); err != nil {
		t.Fatalf("anonymous scope should load its own response: %v", err)
	}
}

func TestOwnerAuthorizedMatrix(t *testing.T) {
	cases := []struct {
		owner, requester string
		authEnabled      bool
		want             bool
	}{
		{"key-A", "key-A", true, true},
		{"key-A", "key-B", true, false},
		{"", "key-A", true, false},        // legacy denied under auth
		{"", anonymousOwner, false, true}, // legacy allowed single-user
		{anonymousOwner, anonymousOwner, false, true},
	}
	for _, c := range cases {
		if got := ownerAuthorized(c.owner, c.requester, c.authEnabled); got != c.want {
			t.Errorf("ownerAuthorized(%q,%q,%v)=%v want %v", c.owner, c.requester, c.authEnabled, got, c.want)
		}
	}
}
