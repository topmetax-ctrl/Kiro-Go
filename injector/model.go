// Package injector reconstructs Kiro IDE / kiro-cli credential stores from a
// Kiro-Go account export so a user can switch the logged-in account on their
// machine without going through the browser login flow again.
//
// It is intended to be compiled into a small local binary (cmd/inject) that the
// user runs on the same machine as the Kiro IDE / kiro-cli. The Kiro-Go server
// itself usually runs in Docker and cannot reach the host filesystem, which is
// why injection lives in a separate local tool rather than a server endpoint.
package injector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ExportData mirrors the JSON produced by the Kiro-Go admin export endpoint
// (proxy/handler.go apiExportAccounts). Only the fields the injector needs are
// modeled; unknown fields are ignored by the JSON decoder.
type ExportData struct {
	Version    string          `json:"version"`
	ExportedAt int64           `json:"exportedAt"`
	Accounts   []ExportAccount `json:"accounts"`
}

// ExportAccount is one account entry in the export.
type ExportAccount struct {
	ID          string            `json:"id"`
	Email       string            `json:"email"`
	Nickname    string            `json:"nickname,omitempty"`
	Idp         string            `json:"idp"`
	UserId      string            `json:"userId,omitempty"`
	MachineId   string            `json:"machineId,omitempty"`
	Credentials ExportCredentials `json:"credentials"`
}

// ExportCredentials carries the secret material plus the metadata needed to
// rebuild a token file. expiresAt is a Unix timestamp in MILLISECONDS (the
// export multiplies the stored seconds by 1000).
type ExportCredentials struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ClientID         string `json:"clientId,omitempty"`
	ClientSecret     string `json:"clientSecret,omitempty"`
	Region           string `json:"region,omitempty"`
	ExpiresAt        int64  `json:"expiresAt"`
	AuthMethod       string `json:"authMethod,omitempty"`
	Provider         string `json:"provider,omitempty"`
	IssuerURL        string `json:"issuerUrl,omitempty"`
	IdPClientID      string `json:"idpClientId,omitempty"`
	Scopes           string `json:"scopes,omitempty"`
	LoginHint        string `json:"loginHint,omitempty"`
	ProfileArn       string `json:"profileArn,omitempty"`
	IdpTokenEndpoint string `json:"idpTokenEndpoint,omitempty"`
}

// NormalizedAuthMethod maps the export's authMethod (which can be "IdC",
// "external_idp", "social", a provider name, etc.) plus the IdP signal fields
// onto one of: "idc", "social", "external_idp". This is the same precedence the
// Kiro-Go importer uses: trust IdP signals over the label.
func (a ExportAccount) NormalizedAuthMethod() string {
	am := strings.ToLower(strings.TrimSpace(a.Credentials.AuthMethod))
	c := a.Credentials
	// IdP signals win regardless of the stored label — an export from an older
	// build can mislabel a Microsoft Entra account as "social".
	if c.IssuerURL != "" || c.IdPClientID != "" ||
		strings.Contains(strings.ToLower(c.Provider), "entra") ||
		strings.Contains(strings.ToLower(c.Provider), "microsoft") {
		return "external_idp"
	}
	switch am {
	case "external_idp", "externalidp":
		return "external_idp"
	case "idc", "builderid", "enterprise", "idcenter":
		return "idc"
	case "social", "google", "github":
		return "social"
	}
	// Fall back to client-credential presence: IdC accounts carry clientId+secret.
	if c.ClientID != "" && c.ClientSecret != "" {
		return "idc"
	}
	return "social"
}

// HasRefresh reports whether a refresh token is present.
func (a ExportAccount) HasRefresh() bool {
	return strings.TrimSpace(a.Credentials.RefreshToken) != ""
}

// LoadFromFile reads an export JSON file from disk.
func LoadFromFile(path string) (*ExportData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read export file: %w", err)
	}
	return parseExport(data)
}

// LoadFromHTTP fetches the export from a running Kiro-Go admin server.
// baseURL is the server origin (e.g. "http://localhost:8080"); adminPassword is
// sent as the X-Admin-Password header that handleAdminAPI checks.
func LoadFromHTTP(baseURL, adminPassword string) (*ExportData, error) {
	url := strings.TrimRight(baseURL, "/") + "/admin/api/export"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Password", adminPassword)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request export: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized: wrong admin password")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("export returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseExport(body)
}

func parseExport(data []byte) (*ExportData, error) {
	var ed ExportData
	if err := json.Unmarshal(data, &ed); err != nil {
		return nil, fmt.Errorf("parse export JSON: %w", err)
	}
	return &ed, nil
}
