package injector

import (
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed web/index.html
var webFS embed.FS

// Server holds the loaded export and serves the local injection UI.
type Server struct {
	mu         sync.Mutex
	export     *ExportData
	cacheDir   string // override for Kiro IDE cache (dry-run); "" = default
	cliDB      string // override for CLI sqlite path (dry-run); "" = default
	profileDir string // override for IDE globalStorage profile dir (dry-run); "" = default
}

// NewServer creates a server. An export may be loaded later via the UI.
func NewServer() *Server { return &Server{} }

// Handler returns the HTTP mux for the local UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/load", s.handleLoad)
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/inject", s.handleInject)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "ui not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleLoad loads an export from a live server or an uploaded file path.
// Body: {"source":"http","baseUrl":"...","password":"..."} or
//
//	{"source":"file","path":"/abs/path.json"}
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source   string `json:"source"`
		BaseURL  string `json:"baseUrl"`
		Password string `json:"password"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var (
		ed  *ExportData
		err error
	)
	switch req.Source {
	case "http":
		ed, err = LoadFromHTTP(req.BaseURL, req.Password)
	case "file":
		ed, err = LoadFromFile(req.Path)
	default:
		writeJSONError(w, http.StatusBadRequest, "source must be 'http' or 'file'")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.mu.Lock()
	s.export = ed
	s.mu.Unlock()

	s.handleAccounts(w, r)
}

// accountView is the non-secret summary the UI lists.
type accountView struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	AuthMethod string `json:"authMethod"`
	Provider   string `json:"provider"`
	HasRefresh bool   `json:"hasRefresh"`
	IDEOK      bool   `json:"ideOk"`
	CLIOK      bool   `json:"cliOk"`
	CLINote    string `json:"cliNote,omitempty"`
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ed := s.export
	s.mu.Unlock()
	if ed == nil {
		writeJSONError(w, http.StatusNotFound, "no export loaded")
		return
	}

	views := make([]accountView, 0, len(ed.Accounts))
	for _, a := range ed.Accounts {
		method := a.NormalizedAuthMethod()
		v := accountView{
			ID:         a.ID,
			Email:      a.Email,
			AuthMethod: method,
			Provider:   a.Credentials.Provider,
			HasRefresh: a.HasRefresh(),
			IDEOK:      true, // IDE supports all three auth methods
		}
		switch method {
		case "external_idp":
			// Verified against kiro-cli 2.10.0 with the correct kirocli: key and
			// shape: the social-token row makes `whoami` report "Logged in", but
			// `chat` then fails with "Authentication failed". The reason is that
			// the Kiro backend only accepts a Microsoft Entra bearer when the
			// request carries the TokenType: EXTERNAL_IDP header — which the CLI
			// sends only for its own native external-idp login, not for an
			// injected token. So External IdP accounts cannot be injected.
			v.CLIOK = false
			v.CLINote = "Microsoft (External IdP) accounts can't be injected into kiro-cli — backend rejects chat (needs the CLI's native external-idp login)"
		default: // social, idc
			// Injected using the social-token shape (the approach proven by the
			// agent-vibes exporter). Requires a profileArn. Not verified
			// end-to-end here (no social/IdC account was available to test).
			if strings.TrimSpace(a.Credentials.ProfileArn) == "" {
				v.CLIOK = false
				v.CLINote = "missing profileArn — kiro-cli requires it"
			} else {
				v.CLIOK = true
			}
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  ed.Version,
		"accounts": views,
	})
}

// handleInject performs the injection for one account into one or both targets.
// Body: {"id":"...","targets":["ide","cli"],"dryRun":true,"relaunch":false}
func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string   `json:"id"`
		Targets  []string `json:"targets"`
		DryRun   bool     `json:"dryRun"`
		Relaunch bool     `json:"relaunch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	s.mu.Lock()
	ed := s.export
	cacheDir, cliDB := s.cacheDir, s.cliDB
	s.mu.Unlock()
	if ed == nil {
		writeJSONError(w, http.StatusNotFound, "no export loaded")
		return
	}

	var acc *ExportAccount
	for i := range ed.Accounts {
		if ed.Accounts[i].ID == req.ID {
			acc = &ed.Accounts[i]
			break
		}
	}
	if acc == nil {
		writeJSONError(w, http.StatusNotFound, "account not found")
		return
	}

	results := map[string]any{}

	for _, t := range req.Targets {
		switch t {
		case "ide":
			dir := cacheDir
			if req.DryRun {
				dir = filepath.Join(os.TempDir(), "kiro-inject-dryrun", "ide")
			}
			written, err := InjectKiroIDE(*acc, dir)
			out := injectOutcome(written, err, req.DryRun)
			// The IDE reads the active profileArn from profile.json in its
			// globalStorage, NOT from the token file. Without updating it, a stale
			// arn from a prior login makes usage/model-list calls 403 (the IDE's
			// ProfileArnGuard only self-heals when the arn is missing, never when
			// it is present-but-wrong). Write it so account switches take effect.
			pDir := s.profileDir
			if req.DryRun {
				pDir = filepath.Join(os.TempDir(), "kiro-inject-dryrun", "profile")
			}
			if pPath, pErr := WriteKiroProfile(*acc, pDir); pErr != nil {
				out["profileError"] = pErr.Error()
			} else if pPath != "" {
				out["profileWritten"] = pPath
			}
			results["ide"] = out
			if err == nil && !req.DryRun && req.Relaunch {
				_ = KillApp(AppKiroIDE)
				if lerr := LaunchKiroIDE(); lerr != nil {
					results["ideLaunch"] = lerr.Error()
				}
			}
		case "cli":
			// External IdP (Microsoft Entra) accounts cannot be injected into the
			// CLI. Verified against kiro-cli 2.10.0: the social-shaped token makes
			// `whoami` report "Logged in", but `chat` then fails with
			// "Authentication failed" — the same Microsoft token works through
			// Kiro-Go because Kiro-Go sends a `TokenType: EXTERNAL_IDP` header
			// (proxy/kiro_api.go) on data-plane calls, which the CLI does not do
			// for an externally injected token. idc / social use the social-token
			// shape (the approach the production agent-vibes exporter settled on).
			if m := acc.NormalizedAuthMethod(); m == "external_idp" {
				results["cli"] = map[string]any{
					"ok":    false,
					"error": "kiro-cli cannot use Microsoft (external_idp) accounts: whoami succeeds but chat fails because the CLI omits the TokenType: EXTERNAL_IDP header the backend requires",
				}
				continue
			}
			dbPath := cliDB
			if req.DryRun {
				dbPath = filepath.Join(os.TempDir(), "kiro-inject-dryrun", "data.sqlite3")
				if err := ensureDryRunCLIDB(dbPath); err != nil {
					results["cli"] = map[string]any{"ok": false, "error": err.Error()}
					continue
				}
			}
			err := InjectKiroCLI(*acc, dbPath)
			out := map[string]any{"ok": err == nil, "dryRun": req.DryRun}
			if err != nil {
				out["error"] = err.Error()
			} else if req.DryRun {
				out["path"] = dbPath
			}
			results["cli"] = out
		default:
			results[t] = map[string]any{"ok": false, "error": "unknown target"}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "results": results})
}

func injectOutcome(written []string, err error, dryRun bool) map[string]any {
	out := map[string]any{"ok": err == nil, "dryRun": dryRun}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["written"] = written
	}
	return out
}

// SetDryRunOverrides lets the entrypoint pin override paths (used by --cache-dir).
// The profile dir tracks the cache dir under the same parent for --cache-dir
// testing so a dry-run never touches the real IDE globalStorage.
func (s *Server) SetDryRunOverrides(cacheDir, cliDB string) {
	s.mu.Lock()
	s.cacheDir, s.cliDB = cacheDir, cliDB
	if cacheDir != "" {
		s.profileDir = filepath.Join(cacheDir, "globalStorage-profile")
	}
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ensureDryRunCLIDB creates a throwaway sqlite db with the auth_kv table so a
// dry-run CLI inject has somewhere to write without touching the real CLI db.
func ensureDryRunCLIDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return createEmptyAuthDB(path)
}
