package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/setup"
)

// The setup surface's logic lives in internal/setup (§3.4): the schema
// description, body parsing/coercion, validation, the atomic save and the
// OIDC discovery probe. These handlers are the HTTP shell only — the
// server-rendered settings page (internal/webui) binds the same functions, so
// the two surfaces cannot drift.

// setupBaseConfig resolves the file-visible base the setup API edits on top
// of (§3.4): defaults ⊕ the first existing candidate config file.
func (s *Server) setupBaseConfig() *config.Config {
	return setup.BaseConfig(s.cfg.DataDir, s.boot.ConfigPaths)
}

// setupGet answers GET /api/v1/setup: full config schema + effective values +
// file state + validation errors (§3.4).
func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	fileState := "absent"
	if s.boot.Mode == "normal" {
		fileState = "valid"
	} else if s.boot.InSetupOnly() {
		fileState = "invalid"
	}
	errs := []setup.Error{}
	for _, e := range s.boot.Errors {
		errs = append(errs, setup.Error{Message: e})
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"file_state": fileState,
		"errors":     errs,
		"groups":     setup.BuildSchema(s.cfg, config.Defaults()),
	})
}

// parseSetupCandidate parses the request body over the file-visible base and
// runs the full validator. On any failure it writes the §3.4 422 body
// ({errors: [{key, message}]}) and reports ok=false — the caller only has to
// decide what success means (respond, or save).
func (s *Server) parseSetupCandidate(w http.ResponseWriter, r *http.Request) (*config.Config, bool) {
	c, err := setup.ParseRequest(r, s.setupBaseConfig())
	if err != nil {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{
			"errors": []setup.Error{{Message: err.Error()}},
		})
		return nil, false
	}
	if errs := setup.Validate(c); len(errs) > 0 {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{"errors": errs})
		return nil, false
	}
	return c, true
}

// setupTest answers POST /api/v1/setup/test: run the full validator WITHOUT
// saving → 200 {errors: []} or 422 {errors: [{key,message}]} (§3.4).
func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	if _, ok := s.parseSetupCandidate(w, r); !ok {
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{"errors": []setup.Error{}})
}

// setupAuthTest answers POST /api/v1/setup/auth/test: probe the OIDC
// discovery document of the issuer in the FORM (not the saved config) so the
// auth card's Test button validates what the operator typed.
func (s *Server) setupAuthTest(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	var req struct {
		Issuer      string `json:"issuer"`
		ClientID    string `json:"client_id"`
		RedirectURI string `json:"redirect_uri"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{
			"errors": []setup.Error{{Message: "body must be JSON {issuer, client_id, redirect_uri}"}},
		})
		return
	}
	probe, errs := setup.ProbeAuth(req.Issuer, req.ClientID, req.RedirectURI)
	if len(errs) > 0 {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{"errors": errs})
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"issuer":                 probe.Issuer,
		"authorization_endpoint": probe.AuthorizationEndpoint,
		"token_endpoint":         probe.TokenEndpoint,
		"jwks_uri":               probe.JWKSURI,
		"scopes_supported":       probe.ScopesSupported,
		"signing_algs":           probe.SigningAlgs,
		"redirect_uri":           probe.RedirectURI,
		"client_id_present":      probe.ClientIDPresent,
	})
}

// setupPut answers PUT /api/v1/setup: validate + atomically write
// <data-dir>/walhub.toml, then respond 200 {saved, requires_restart, errors}
// (§3.4).
func (s *Server) setupPut(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	c, ok := s.parseSetupCandidate(w, r)
	if !ok {
		return
	}
	restart, serr := setup.Save(s.cfg.DataDir, c, s.cfg)
	if serr != nil {
		writeJSONBody(w, http.StatusInternalServerError, map[string]any{
			"errors": []setup.Error{{Message: serr.Error()}},
		})
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"saved":            true,
		"requires_restart": restart,
		"errors":           []setup.Error{},
	})
}
