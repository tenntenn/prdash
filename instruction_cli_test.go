package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestInstructionCLIUsesServerAndRefreshesRejectedAuthentication(t *testing.T) {
	t.Setenv("PRDASH_API_TOKEN", "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth" {
			if r.Header.Get("X-Prdash-Client") != "dashboard" {
				t.Error("missing bootstrap header")
			}
			_, _ = w.Write([]byte(`{"token":"server-token"}`))
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/sessions/worker/instructions/instruction-1/claim" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing JSON content type")
		}
		if r.Header.Get("Authorization") != "Bearer server-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_required"}`))
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["consumer_id"] != "consumer" {
			t.Error("missing consumer ID")
		}
		_, _ = w.Write([]byte(`{"claim_token":"capability","instruction":{"attempt":1}}`))
	}))
	defer srv.Close()
	var out, stderr bytes.Buffer
	err := runInstructionCmd(t.Context(), []string{"claim", "-server", srv.URL, "-session", "worker", "-id", "instruction-1", "-consumer", "consumer"}, &out, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("operation calls = %d", calls.Load())
	}
	if !bytes.Contains(out.Bytes(), []byte("capability")) {
		t.Fatalf("missing claim response: %s", out.String())
	}
}

func TestInstructionCLIDoesNotRetryAmbiguousFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"server failure", http.StatusInternalServerError, `{"error":"save failed"}`},
		{"unrecognized unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			var out, stderr bytes.Buffer
			err := runInstructionCmd(t.Context(), []string{"claim", "-server", srv.URL, "-session", "worker", "-id", "instruction-1", "-consumer", "consumer"}, &out, &stderr)
			if err == nil {
				t.Fatal("failed operation succeeded")
			}
			if calls.Load() != 1 {
				t.Fatalf("ambiguous operation retried: %d", calls.Load())
			}
		})
	}
}
