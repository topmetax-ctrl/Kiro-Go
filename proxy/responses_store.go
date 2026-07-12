package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	responsesDirName    = "responses"
	responsesDefaultTTL = 30 * 24 * time.Hour
)

func responsesDir() string {
	return filepath.Join(config.GetConfigDir(), responsesDirName)
}

func generateResponseID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("resp_%d%012x", time.Now().UnixNano(), 0)
	}
	return "resp_" + hex.EncodeToString(buf) + fmt.Sprintf("%08x", time.Now().Unix()&0xffffffff)
}

func generateOutputItemID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// saveResponse persists a response scoped to ownerPrincipalID. createdByKeyID is
// recorded for audit (may equal ownerPrincipalID). When auth is disabled the
// caller passes anonymousOwner.
func saveResponse(resp *ResponsesObject, ownerPrincipalID, createdByKeyID string) error {
	if resp == nil || resp.ID == "" {
		return fmt.Errorf("response missing id")
	}
	dir := responsesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create responses dir: %w", err)
	}
	if resp.StoredAt == 0 {
		resp.StoredAt = time.Now().Unix()
	}

	persisted := storedResponseDoc{
		ID:                 resp.ID,
		Object:             resp.Object,
		CreatedAt:          resp.CreatedAt,
		Status:             resp.Status,
		Model:              resp.Model,
		Output:             resp.Output,
		Usage:              resp.Usage,
		PreviousResponseID: resp.PreviousResponseID,
		Metadata:           resp.Metadata,
		Instructions:       resp.Instructions,
		StoredInput:        resp.StoredInput,
		StoredAt:           resp.StoredAt,
		OwnerPrincipalID:   ownerPrincipalID,
		CreatedByKeyID:     createdByKeyID,
	}

	path := filepath.Join(dir, sanitizeResponseID(resp.ID)+".json")
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stored response: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write stored response: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit stored response: %w", err)
	}
	return nil
}

// loadResponseForOwner loads a stored response only if it belongs to the given
// principal. requesterID is the caller's principal (ApiKeyEntry.ID), or
// anonymousOwner when auth is disabled. authEnabled distinguishes the two
// deployment modes so legacy (owner-empty) docs are handled correctly:
//   - owner matches requester            → allowed
//   - owner differs                      → generic not-found (no existence leak)
//   - legacy empty owner + auth enabled  → denied (do NOT let the first accessor
//     adopt an orphaned conversation)
//   - legacy empty owner + auth disabled → allowed (single anonymous scope)
//
// Expired docs are pruned and reported as not-found.
func loadResponseForOwner(id, requesterID string, authEnabled bool) (*ResponsesObject, error) {
	doc, path, err := loadResponseDoc(id)
	if err != nil {
		return nil, err
	}
	if doc.StoredAt > 0 && time.Since(time.Unix(doc.StoredAt, 0)) > responsesDefaultTTL {
		_ = os.Remove(path)
		return nil, errResponseNotFound
	}

	if !ownerAuthorized(doc.OwnerPrincipalID, requesterID, authEnabled) {
		return nil, errResponseNotFound
	}

	return docToResponsesObject(doc), nil
}

// ownerAuthorized applies the ownership policy described on loadResponseForOwner.
func ownerAuthorized(owner, requester string, authEnabled bool) bool {
	if owner == "" {
		// Legacy doc with no owner. Only reachable in single-user (auth-off) mode.
		return !authEnabled
	}
	return owner == requester
}

// loadResponseDoc reads and decodes a stored response document (no ownership or
// TTL checks). Returns the on-disk path for pruning.
func loadResponseDoc(id string) (storedResponseDoc, string, error) {
	if id == "" {
		return storedResponseDoc{}, "", fmt.Errorf("empty response id")
	}
	path := filepath.Join(responsesDir(), sanitizeResponseID(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return storedResponseDoc{}, path, errResponseNotFound
		}
		return storedResponseDoc{}, path, err
	}
	var doc storedResponseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return storedResponseDoc{}, path, fmt.Errorf("decode stored response: %w", err)
	}
	return doc, path, nil
}

func docToResponsesObject(doc storedResponseDoc) *ResponsesObject {
	return &ResponsesObject{
		ID:                 doc.ID,
		Object:             doc.Object,
		CreatedAt:          doc.CreatedAt,
		Status:             doc.Status,
		Model:              doc.Model,
		Output:             doc.Output,
		Usage:              doc.Usage,
		PreviousResponseID: doc.PreviousResponseID,
		Metadata:           doc.Metadata,
		Instructions:       doc.Instructions,
		StoredInput:        doc.StoredInput,
		StoredAt:           doc.StoredAt,
	}
}

func purgeExpiredResponses(ttl time.Duration) {
	if ttl <= 0 {
		ttl = responsesDefaultTTL
	}
	dir := responsesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(full); err != nil {
				logger.Warnf("[Responses] purge %s failed: %v", e.Name(), err)
			}
		}
	}
}

func logResponsesPersistFailure(id string, err error) {
	logger.Warnf("[Responses] persist %s failed: %v", id, err)
}

func sanitizeResponseID(id string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return -1
		}
	}, id)
	if cleaned == "" {
		return "invalid"
	}
	return cleaned
}

type storedResponseDoc struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	Usage              ResponsesUsage       `json:"usage"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
	Instructions       string               `json:"instructions,omitempty"`
	StoredInput        json.RawMessage      `json:"stored_input,omitempty"`
	StoredAt           int64                `json:"stored_at"`

	// OwnerPrincipalID scopes a stored response to the principal that created it,
	// so one API key cannot read another key's conversation via
	// previous_response_id. The owner is the stable ApiKeyEntry.ID (survives raw
	// key rotation-in-place). CreatedByKeyID is kept for audit only.
	//
	// Legacy docs written before this field exists have an empty OwnerPrincipalID;
	// see loadResponseForOwner for how they are handled (denied under auth, not
	// adopted by the first accessor).
	OwnerPrincipalID string `json:"owner_principal_id,omitempty"`
	CreatedByKeyID   string `json:"created_by_key_id,omitempty"`
}

// anonymousOwner is the owner assigned when API-key auth is disabled: the proxy is
// effectively single-user, so all stored responses share one anonymous scope.
const anonymousOwner = "anonymous"

// errResponseNotFound is the single generic error returned for both "no such
// stored response" and "owned by a different principal", so a caller cannot probe
// which response IDs exist.
var errResponseNotFound = fmt.Errorf("stored response not found")
