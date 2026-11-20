package server

// The setup API's wire shape is frozen (06_server_http.md §3.4): the schema
// groups and their field rows, in declaration order, with typed values. The
// schema/coercion logic lives in internal/setup — these tests pin what the
// HTTP surface emits so the shared implementation can move without the JSON
// drifting (the server-rendered /setup page binds the same functions).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crueber/walhub/internal/config"
)

func TestSetupAPISchemaWireShape(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "normal"}})
	rec := httptest.NewRecorder()
	s.setupGet(rec, httptest.NewRequest("GET", "/api/v1/setup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/setup = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Top level: errors, file_state, groups (map keys marshal sorted).
	if want := `{"errors":[],"file_state":"valid","groups":[`; !strings.HasPrefix(body, want) {
		t.Fatalf("body does not start with %s: %.200s", want, body)
	}
	// Row shape + key order: key, value, default, type, secret, doc.
	row := `{"key":"server.listen","value":"` + cfg.Server.Listen + `","default":"` + config.Defaults().Server.Listen +
		`","type":"string","secret":false,"doc":"Listen"}`
	if !strings.Contains(body, row) {
		t.Fatalf("body missing the frozen row %s", row)
	}
	// The auth subsection is its own card, immediately after server.
	if !strings.Contains(body, `},{"section":"auth","keys":[{"key":"server.auth.mode"`) {
		t.Fatalf("auth card is not the second group: %.400s", body)
	}
	// Typed values keep their JSON types: a duration renders as its Go string,
	// a byte size as a bare number.
	var payload struct {
		Groups []struct {
			Section string `json:"section"`
			Keys    []struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
				Type  string          `json:"type"`
			} `json:"keys"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, g := range payload.Groups {
		for _, k := range g.Keys {
			seen[k.Key] = k.Type + "=" + string(k.Value)
		}
	}
	if got := seen["server.request_timeout"]; got != `duration="`+cfg.Server.RequestTimeout.String()+`"` {
		t.Fatalf("duration row = %s", got)
	}
	if got := seen["server.max_push_bytes"]; got != "int="+strconv.FormatInt(int64(cfg.Server.MaxPushBytes), 10) {
		t.Fatalf("byte size must marshal as a bare number, got %s", got)
	}
}

func TestSetupAPISaveResponseWireShape(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.FirstRunDefaults(dataDir)
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		DataDir: dataDir, Boot: BootState{Mode: "normal"}})

	req := httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.listen": "127.0.0.1:9099"}}`))
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, `{"errors":[],"requires_restart":["server.listen"],"saved":true}`) {
		t.Fatalf("save body = %s", body)
	}

	// Validation failures report EVERY error as {key, message}.
	req = httptest.NewRequest("POST", "/api/v1/setup/test",
		strings.NewReader(`{"overrides": {"store.backend": "nope", "server.max_push_bytes": "-1"}}`))
	rec = httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Errors []struct {
			Key     string `json:"key"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) < 2 {
		t.Fatalf("errors = %+v, want every validation error", out.Errors)
	}
	for _, e := range out.Errors {
		if e.Message == "" {
			t.Fatalf("empty message in %+v", out.Errors)
		}
	}
}
