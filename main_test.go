// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"net/http"
	"testing"
)

// listen's local path should return a working plain-HTTP listener and a cleanup
// that closes it.
func TestListen_LocalAddr(t *testing.T) {
	ln, cleanup, err := listen("127.0.0.1:0", ":80", "ignored", "", quietLog())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cleanup()

	if ln.Addr().Network() != "tcp" {
		t.Errorf("network = %q, want tcp", ln.Addr().Network())
	}

	// Serve one request to prove the listener is live.
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}()
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestModeAndAddrHelpers(t *testing.T) {
	if modeName("127.0.0.1:80") != "local" || modeName("") != "tsnet" {
		t.Error("modeName mapping wrong")
	}
	if listenAddr("127.0.0.1:9", ":80") != "127.0.0.1:9" || listenAddr("", ":80") != ":80" {
		t.Error("listenAddr mapping wrong")
	}
}
