package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptrF(f float64) *float64 { return &f }

func TestSettings_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CompressSettings)
		wantErr bool
	}{
		{"defaults ok", func(*CompressSettings) {}, false},
		{"negative protect_recent", func(s *CompressSettings) { s.ProtectRecent = -1 }, true},
		{"negative min_tokens", func(s *CompressSettings) { s.MinTokensToCompress = -5 }, true},
		{"target_ratio zero", func(s *CompressSettings) { s.TargetRatio = ptrF(0) }, true},
		{"target_ratio above 1", func(s *CompressSettings) { s.TargetRatio = ptrF(1.5) }, true},
		{"target_ratio valid", func(s *CompressSettings) { s.TargetRatio = ptrF(0.5) }, false},
		{"empty kompress_model", func(s *CompressSettings) { e := ""; s.KompressModel = &e }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultSettings()
			tc.mutate(&s)
			err := s.validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestSettings_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json") // nested dir must be created
	st := loadSettings(path, quietLog())

	s := defaultSettings()
	s.CompressUserMessages = true
	s.ProtectRecent = 0
	s.TargetRatio = ptrF(0.3)
	if err := st.set(s); err != nil {
		t.Fatalf("set: %v", err)
	}

	// A fresh store from the same path must see the persisted values.
	st2 := loadSettings(path, quietLog())
	got := st2.get()
	if !got.CompressUserMessages || got.ProtectRecent != 0 || got.TargetRatio == nil || *got.TargetRatio != 0.3 {
		t.Fatalf("reloaded settings mismatch: %+v", got)
	}
}

func TestSettings_LoadMissingAndCorrupt(t *testing.T) {
	// Missing file -> defaults.
	missing := loadSettings(filepath.Join(t.TempDir(), "nope.json"), quietLog()).get()
	if missing != defaultSettings() {
		t.Errorf("missing file should yield defaults, got %+v", missing)
	}

	// Corrupt file -> defaults, no crash.
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings(path, quietLog()).get(); got != defaultSettings() {
		t.Errorf("corrupt file should yield defaults, got %+v", got)
	}
}

func TestSettings_SetRejectsInvalidAndKeepsCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	st := loadSettings(path, quietLog())

	bad := defaultSettings()
	bad.ProtectRecent = -10
	if err := st.set(bad); err == nil {
		t.Fatal("expected set to reject invalid settings")
	}
	if st.get() != defaultSettings() {
		t.Errorf("invalid set must not change current settings, got %+v", st.get())
	}
	// And nothing should have been written to disk.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("invalid set must not write the config file")
	}
}

func doConfig(t *testing.T, h *configHandler, method, body string) (*httptest.ResponseRecorder, CompressSettings) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/config", nil)
	} else {
		r = httptest.NewRequest(method, "/config", strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out CompressSettings
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestConfigHandler_GetPutMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	st := loadSettings(path, quietLog())
	h := &configHandler{store: st, log: quietLog()}

	// GET returns defaults.
	rec, got := doConfig(t, h, http.MethodGet, "")
	if rec.Code != http.StatusOK || got != defaultSettings() {
		t.Fatalf("GET: code=%d settings=%+v", rec.Code, got)
	}

	// PUT a partial update: only flip compress_user_messages.
	rec, got = doConfig(t, h, http.MethodPut, `{"compress_user_messages": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !got.CompressUserMessages {
		t.Errorf("compress_user_messages not applied: %+v", got)
	}
	// Untouched fields keep defaults (merge, not replace).
	if got.ProtectRecent != 4 || !got.CompressSystemMessages {
		t.Errorf("partial PUT clobbered other fields: %+v", got)
	}
	// Persisted to disk.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("PUT did not persist config file: %v", err)
	}

	// A second partial PUT merges onto the first.
	_, got = doConfig(t, h, http.MethodPut, `{"protect_recent": 0}`)
	if !got.CompressUserMessages || got.ProtectRecent != 0 {
		t.Errorf("second PUT did not merge onto prior state: %+v", got)
	}
}

// target_ratio is a nullable pointer field; exercise set-to-value, preserve on
// an unrelated partial PUT, and explicit set-back-to-null.
func TestConfigHandler_NullableMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	st := loadSettings(path, quietLog())
	h := &configHandler{store: st, log: quietLog()}

	if st.get().TargetRatio != nil {
		t.Fatalf("default target_ratio should be null, got %v", *st.get().TargetRatio)
	}

	// Set it.
	_, got := doConfig(t, h, http.MethodPut, `{"target_ratio": 0.3}`)
	if got.TargetRatio == nil || *got.TargetRatio != 0.3 {
		t.Fatalf("target_ratio not set: %+v", got.TargetRatio)
	}

	// An unrelated partial PUT must preserve it.
	_, got = doConfig(t, h, http.MethodPut, `{"protect_recent": 2}`)
	if got.TargetRatio == nil || *got.TargetRatio != 0.3 {
		t.Errorf("unrelated PUT clobbered target_ratio: %+v", got.TargetRatio)
	}

	// Explicit null clears it.
	_, got = doConfig(t, h, http.MethodPut, `{"target_ratio": null}`)
	if got.TargetRatio != nil {
		t.Errorf("explicit null did not clear target_ratio: %v", *got.TargetRatio)
	}
}

func TestConfigHandler_PutInvalid(t *testing.T) {
	st := loadSettings(filepath.Join(t.TempDir(), "config.json"), quietLog())
	h := &configHandler{store: st, log: quietLog()}

	// Invalid JSON -> 400, unchanged.
	rec, _ := doConfig(t, h, http.MethodPut, `{nope}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON code=%d, want 400", rec.Code)
	}
	// Valid JSON but invalid value -> 400, unchanged.
	rec, _ = doConfig(t, h, http.MethodPut, `{"target_ratio": 9}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid value code=%d, want 400", rec.Code)
	}
	if st.get() != defaultSettings() {
		t.Errorf("rejected PUTs must not change settings: %+v", st.get())
	}
}

func TestConfigHandler_RejectsDelete(t *testing.T) {
	st := loadSettings("", quietLog())
	h := &configHandler{store: st, log: quietLog()}
	rec, _ := doConfig(t, h, http.MethodDelete, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE code=%d, want 405", rec.Code)
	}
}
