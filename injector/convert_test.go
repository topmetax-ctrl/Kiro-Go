package injector

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMsToISO(t *testing.T) {
	// 1782733569000 ms = 2026-06-29T... (the exported expiresAt seen in config)
	got := msToISO(1700000000000)
	want := "2023-11-14T22:13:20.000Z"
	if got != want {
		t.Fatalf("msToISO = %q, want %q", got, want)
	}
	if msToISO(0) != "" {
		t.Fatalf("msToISO(0) should be empty")
	}
	if msToISO(-5) != "" {
		t.Fatalf("msToISO(neg) should be empty")
	}
}

func TestMsToRFC3339(t *testing.T) {
	got := msToRFC3339(1700000000000)
	want := "2023-11-14T22:13:20Z"
	if got != want {
		t.Fatalf("msToRFC3339 = %q, want %q", got, want)
	}
	if msToRFC3339(0) != "" {
		t.Fatalf("msToRFC3339(0) should be empty")
	}
}

func TestSha1Hex(t *testing.T) {
	in := "my-client-id-123"
	sum := sha1.Sum([]byte(in))
	want := hex.EncodeToString(sum[:])
	if got := sha1Hex(in); got != want {
		t.Fatalf("sha1Hex = %q, want %q", got, want)
	}
	// known vector: sha1("") = da39a3ee5e6b4b0d3255bfef95601890afd80709
	if got := sha1Hex(""); got != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Fatalf("sha1Hex empty = %q", got)
	}
}

func TestRegExpiryFromSecret(t *testing.T) {
	// Build a fake JWT whose payload carries serialized.expirationTimestamp.
	mkJWT := func(payload map[string]any) string {
		body, _ := json.Marshal(payload)
		enc := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(body)
		return "header." + enc + ".sig"
	}
	ts := float64(1893456000) // 2030-01-01T00:00:00Z
	inner, _ := json.Marshal(map[string]any{"expirationTimestamp": ts})
	jwt := mkJWT(map[string]any{"serialized": string(inner)})
	got := regExpiryFromSecret(jwt)
	if !strings.HasPrefix(got, "2030-01-01T00:00:00") {
		t.Fatalf("regExpiryFromSecret = %q, want 2030-01-01...", got)
	}

	// Garbage secret → fallback ~ now+80d (just assert it parses and is in the future).
	fb := regExpiryFromSecret("not-a-jwt")
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", fb)
	if err != nil {
		t.Fatalf("fallback not parseable: %v", err)
	}
	if !parsed.After(time.Now().Add(70 * 24 * time.Hour)) {
		t.Fatalf("fallback %v not ~80d in future", parsed)
	}
}

func TestNormalizedAuthMethod(t *testing.T) {
	cases := []struct {
		name string
		acc  ExportAccount
		want string
	}{
		{
			name: "microsoft entra via issuerUrl",
			acc: ExportAccount{Credentials: ExportCredentials{
				AuthMethod: "social", // mislabeled
				IssuerURL:  "https://login.microsoftonline.com/x/v2.0",
			}},
			want: "external_idp",
		},
		{
			name: "provider microsoft",
			acc:  ExportAccount{Credentials: ExportCredentials{Provider: "MicrosoftEntra"}},
			want: "external_idp",
		},
		{
			name: "idc label",
			acc:  ExportAccount{Credentials: ExportCredentials{AuthMethod: "IdC"}},
			want: "idc",
		},
		{
			name: "idc via client creds",
			acc:  ExportAccount{Credentials: ExportCredentials{ClientID: "c", ClientSecret: "s"}},
			want: "idc",
		},
		{
			name: "social default",
			acc:  ExportAccount{Credentials: ExportCredentials{AuthMethod: "social", Provider: "Google"}},
			want: "social",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acc.NormalizedAuthMethod(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildKiroIDEToken_ExternalIdP(t *testing.T) {
	acc := ExportAccount{
		Email: "u@example.com",
		Credentials: ExportCredentials{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			AuthMethod:   "external_idp",
			Provider:     "MicrosoftEntra",
			IssuerURL:    "https://login.microsoftonline.com/tenant/v2.0",
			IdPClientID:  "idp-client",
			Scopes:       "api://x/codewhisperer:conversations offline_access",
			LoginHint:    "u@example.com",
			ProfileArn:   "arn:aws:codewhisperer:us-east-1:1:profile/X",
			Region:       "us-east-1",
		},
	}
	tok, regName, reg := buildKiroIDEToken(acc)
	if tok.AuthMethod != "external_idp" {
		t.Fatalf("authMethod = %q", tok.AuthMethod)
	}
	if tok.IssuerURL == "" || tok.IdPClientID == "" || tok.Scopes == "" || tok.LoginHint == "" {
		t.Fatalf("external IdP fields missing: %+v", tok)
	}
	if tok.ProfileArn == "" {
		t.Fatalf("profileArn missing")
	}
	if tok.ExpiresAt != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("expiresAt = %q", tok.ExpiresAt)
	}
	if tok.ClientIDHash != "" || regName != "" || reg != nil {
		t.Fatalf("external IdP must not produce a registration file")
	}
}

func TestBuildKiroIDEToken_IdC(t *testing.T) {
	acc := ExportAccount{
		Credentials: ExportCredentials{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			AuthMethod:   "IdC",
			ClientID:     "client-xyz",
			ClientSecret: "header.payload.sig",
			Region:       "eu-central-1",
		},
	}
	tok, regName, reg := buildKiroIDEToken(acc)
	if tok.AuthMethod != "IdC" {
		t.Fatalf("authMethod = %q", tok.AuthMethod)
	}
	wantHash := sha1Hex("client-xyz")
	if tok.ClientIDHash != wantHash {
		t.Fatalf("clientIdHash = %q, want %q", tok.ClientIDHash, wantHash)
	}
	if regName != wantHash+".json" {
		t.Fatalf("regName = %q", regName)
	}
	if reg == nil || reg.ClientID != "client-xyz" || reg.ClientSecret != "header.payload.sig" {
		t.Fatalf("registration content wrong: %+v", reg)
	}
	if tok.Region != "eu-central-1" {
		t.Fatalf("region = %q", tok.Region)
	}
}

func TestBuildKiroIDEToken_Social(t *testing.T) {
	acc := ExportAccount{
		Credentials: ExportCredentials{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			AuthMethod:   "social",
			Provider:     "Google",
		},
	}
	tok, regName, reg := buildKiroIDEToken(acc)
	if tok.AuthMethod != "social" {
		t.Fatalf("authMethod = %q", tok.AuthMethod)
	}
	if tok.ClientIDHash != "" || regName != "" || reg != nil {
		t.Fatalf("social must not produce registration")
	}
	if tok.Provider != "Google" {
		t.Fatalf("provider = %q", tok.Provider)
	}
}

func TestBuildCLISocialToken(t *testing.T) {
	acc := ExportAccount{
		Credentials: ExportCredentials{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			Region:       "us-east-1",
			AuthMethod:   "social",
			Provider:     "Google",
			ProfileArn:   "arn:aws:codewhisperer:us-east-1:1:profile/X",
		},
	}
	value, err := buildCLISocialToken(acc)
	if err != nil {
		t.Fatal(err)
	}
	var tok cliSocialToken
	if err := json.Unmarshal([]byte(value), &tok); err != nil {
		t.Fatalf("cli social token not valid JSON: %v", err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Fatalf("token content wrong: %+v", tok)
	}
	if tok.ExpiresAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("expires_at = %q", tok.ExpiresAt)
	}
	if tok.Provider != "google" {
		t.Fatalf("provider = %q", tok.Provider)
	}
	if tok.ProfileArn != "arn:aws:codewhisperer:us-east-1:1:profile/X" {
		t.Fatalf("profile_arn = %q", tok.ProfileArn)
	}
}

func TestBuildCLISocialTokenRequiresProfileArn(t *testing.T) {
	acc := ExportAccount{Credentials: ExportCredentials{
		AccessToken: "at", RefreshToken: "rt", // no profileArn
	}}
	if _, err := buildCLISocialToken(acc); err == nil {
		t.Fatalf("expected error when profileArn is missing")
	}
}

func TestNormalizeCLISocialProvider(t *testing.T) {
	if got := normalizeCLISocialProvider("GitHub"); got != "github" {
		t.Fatalf("GitHub → %q", got)
	}
	if got := normalizeCLISocialProvider("MicrosoftEntra"); got != "google" {
		t.Fatalf("non-github should fall back to google, got %q", got)
	}
}

func TestBuildCLIProfile(t *testing.T) {
	acc := ExportAccount{Credentials: ExportCredentials{
		ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/X",
	}}
	value, err := buildCLIProfile(acc)
	if err != nil {
		t.Fatal(err)
	}
	var p cliProfile
	if err := json.Unmarshal([]byte(value), &p); err != nil {
		t.Fatalf("profile not valid JSON: %v", err)
	}
	if p.Arn != "arn:aws:codewhisperer:us-east-1:1:profile/X" {
		t.Fatalf("arn = %q", p.Arn)
	}
	if p.ProfileName != "Social_Default_Profile" {
		t.Fatalf("profile_name = %q", p.ProfileName)
	}
}
