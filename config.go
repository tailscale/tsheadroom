package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// CompressSettings are the runtime-tunable knobs forwarded to headroom.compress.
// They mirror headroom's CompressConfig field names exactly (the worker passes
// them straight through as kwargs), and default to headroom's own defaults so
// behavior is unchanged until someone tunes them.
//
// TargetRatio and KompressModel are pointers because their "unset" value is
// JSON null with real meaning (target_ratio null = "model decides ~15%";
// kompress_model null = headroom's default model).
type CompressSettings struct {
	CompressUserMessages   bool     `json:"compress_user_messages"`
	CompressSystemMessages bool     `json:"compress_system_messages"`
	ProtectRecent          int      `json:"protect_recent"`
	ProtectAnalysisContext bool     `json:"protect_analysis_context"`
	TargetRatio            *float64 `json:"target_ratio"`
	MinTokensToCompress    int      `json:"min_tokens_to_compress"`
	KompressModel          *string  `json:"kompress_model"`
}

// defaultSettings mirrors headroom's CompressConfig defaults (compress.py).
func defaultSettings() CompressSettings {
	return CompressSettings{
		CompressUserMessages:   false,
		CompressSystemMessages: true,
		ProtectRecent:          4,
		ProtectAnalysisContext: true,
		TargetRatio:            nil,
		MinTokensToCompress:    250,
		KompressModel:          nil,
	}
}

// validate rejects nonsensical settings so a bad PUT can't wedge the service.
func (s CompressSettings) validate() error {
	if s.ProtectRecent < 0 {
		return fmt.Errorf("protect_recent must be >= 0")
	}
	if s.MinTokensToCompress < 0 {
		return fmt.Errorf("min_tokens_to_compress must be >= 0")
	}
	if s.TargetRatio != nil && (*s.TargetRatio <= 0 || *s.TargetRatio > 1) {
		return fmt.Errorf("target_ratio must be in (0, 1] or null")
	}
	if s.KompressModel != nil && *s.KompressModel == "" {
		return fmt.Errorf("kompress_model must be a non-empty string or null")
	}
	return nil
}

// settingsStore holds the current settings behind an atomic pointer (read once
// per request, swapped on update) and persists them to disk so the service
// starts up with the last state.
type settingsStore struct {
	cur  atomic.Pointer[CompressSettings]
	path string // "" disables persistence
	log  *slog.Logger

	saveMu sync.Mutex // serializes file writes
}

// loadSettings builds a store, seeding it from path if present and valid,
// otherwise from defaults. A missing or corrupt file is non-fatal.
func loadSettings(path string, log *slog.Logger) *settingsStore {
	st := &settingsStore{path: path, log: log}
	s := defaultSettings()

	if path != "" {
		switch b, err := os.ReadFile(path); {
		case err == nil:
			var loaded CompressSettings
			if jerr := json.Unmarshal(b, &loaded); jerr != nil {
				log.Warn("config file unparseable; using defaults", "path", path, "err", jerr)
			} else if verr := loaded.validate(); verr != nil {
				log.Warn("config file invalid; using defaults", "path", path, "err", verr)
			} else {
				s = loaded
				log.Info("loaded config", "path", path)
			}
		case os.IsNotExist(err):
			log.Info("no config file yet; using defaults", "path", path)
		default:
			log.Warn("config file unreadable; using defaults", "path", path, "err", err)
		}
	}

	st.cur.Store(&s)
	return st
}

// get returns a snapshot of the current settings.
func (st *settingsStore) get() CompressSettings { return *st.cur.Load() }

// set validates, persists, then swaps in new settings. On a persistence error
// the in-memory settings are left unchanged so disk and memory stay consistent.
func (st *settingsStore) set(s CompressSettings) error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := st.save(s); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	st.cur.Store(&s)
	return nil
}

// save atomically writes settings to disk (temp file + rename).
func (st *settingsStore) save(s CompressSettings) error {
	if st.path == "" {
		return nil
	}
	st.saveMu.Lock()
	defer st.saveMu.Unlock()

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(st.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// configHandler serves the runtime tuning API: GET returns the current
// settings; PUT merges the provided fields onto the current settings (partial
// updates allowed), validates, persists, and returns the result.
type configHandler struct {
	store *settingsStore
	log   *slog.Logger
}

const maxConfigBody = 64 << 10

func (h *configHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.store.get())

	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBody))
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		// Merge: unmarshal onto a copy of current settings so omitted fields
		// keep their existing values (partial updates).
		merged := h.store.get()
		if err := json.Unmarshal(body, &merged); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := h.store.set(merged); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.log.Info("config updated", "settings", merged)
		writeJSON(w, http.StatusOK, h.store.get())

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
