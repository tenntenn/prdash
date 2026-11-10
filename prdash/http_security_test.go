package prdash_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func acquireHTTPToken(t *testing.T, base string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Prdash-Client", "dashboard")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token response: %s", resp.Status)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("token response is cacheable")
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Token) != 64 {
		t.Fatal("unexpected token length")
	}
	return data.Token
}

func TestHTTPMutationProtection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, host, origin, fetchSite, auth, contentType, body string
		want                                                   int
	}{
		{"local API client", "", "", "", "valid", "application/json", `{"text":"sample-instruction"}`, 200},
		{"same origin browser", "", "same", "same-origin", "valid", "application/json", `{"text":"sample-instruction"}`, 200},
		{"JSON charset", "", "", "", "valid", "application/json; charset=utf-8", `{"text":"sample-instruction"}`, 200},
		{"missing token", "", "", "", "", "application/json", `{"text":"sample-instruction"}`, 401},
		{"wrong token", "", "", "", "wrong", "application/json", `{"text":"sample-instruction"}`, 401},
		{"missing Bearer scheme", "", "", "", "raw", "application/json", `{"text":"sample-instruction"}`, 401},
		{"foreign host", "example.com", "", "", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"rebinding hostname", "localhost.example.com", "", "", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"foreign origin", "", "https://example.com", "", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"null origin", "", "null", "", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"different local port", "", "http://127.0.0.1:1", "", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"cross-site fetch", "", "", "cross-site", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"same-site fetch", "", "", "same-site", "valid", "application/json", `{"text":"sample-instruction"}`, 403},
		{"text plain JSON", "", "", "", "valid", "text/plain", `{"text":"sample-instruction"}`, 415},
		{"missing content type", "", "", "", "valid", "", `{"text":"sample-instruction"}`, 415},
		{"invalid JSON", "", "", "", "valid", "application/json", `{`, 400},
		{"trailing JSON", "", "", "", "valid", "application/json", `{"text":"sample-instruction"}{}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newSeededService(t)
			srv := httptest.NewServer(prdash.NewHandler(svc))
			defer srv.Close()
			token := acquireHTTPToken(t, srv.URL)
			before := len(svc.Snapshot().ControllerSession.Messages)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/controller/instruction", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			origin := tc.origin
			if origin == "same" {
				origin = srv.URL
			}
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("Content-Type", tc.contentType)
			switch tc.auth {
			case "valid":
				req.Header.Set("Authorization", "Bearer "+token)
			case "wrong":
				req.Header.Set("Authorization", "Bearer sample-invalid-token")
			case "raw":
				req.Header.Set("Authorization", token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			after := len(svc.Snapshot().ControllerSession.Messages)
			if tc.want == 200 && after != before+1 {
				t.Fatalf("accepted operation count=%d, want 1", after-before)
			}
			if tc.want != 200 && after != before {
				t.Fatalf("rejected request changed state: messages %d -> %d", before, after)
			}
		})
	}
}

func TestHTTPTokenBootstrapProtection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, host, origin, site, client string
		want                             int
	}{
		{"local client", "127.0.0.1:1234", "", "", "dashboard", 200},
		{"same origin browser", "localhost:1234", "http://localhost:1234", "same-origin", "dashboard", 200},
		{"IPv6 loopback", "[::1]:1234", "", "", "dashboard", 200},
		{"missing custom header", "127.0.0.1:1234", "", "", "", 403},
		{"foreign host", "example.com", "", "", "dashboard", 403},
		{"foreign origin", "127.0.0.1:1234", "https://example.com", "", "dashboard", 403},
		{"cross-site", "127.0.0.1:1234", "", "cross-site", "dashboard", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := prdash.NewHandler(newSeededService(t))
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/auth", nil)
			req.RemoteAddr = "127.0.0.1:2345"
			req.Header.Set("Origin", tc.origin)
			if tc.origin == "" {
				req.Header.Del("Origin")
			}
			req.Header.Set("Sec-Fetch-Site", tc.site)
			req.Header.Set("X-Prdash-Client", tc.client)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
			if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("bootstrap exposed CORS access")
			}
		})
	}
}

func TestHTTPTokenRotationRejectsBeforeExecution(t *testing.T) {
	t.Parallel()
	svc := newSeededService(t)
	var handler atomic.Value
	handler.Store(prdash.NewHandler(svc))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }))
	defer srv.Close()
	oldToken := acquireHTTPToken(t, srv.URL)
	handler.Store(prdash.NewHandler(svc))
	before := len(svc.Snapshot().ControllerSession.Messages)
	post := func(token string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/controller/instruction", strings.NewReader(`{"text":"sample-instruction"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 {
			var failure struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != "authentication_required" {
				t.Fatalf("unexpected rejection code: %s", failure.Code)
			}
		}
		return resp.StatusCode
	}
	if got := post(oldToken); got != 401 {
		t.Fatalf("stale token status=%d", got)
	}
	if len(svc.Snapshot().ControllerSession.Messages) != before {
		t.Fatal("stale-token request executed")
	}
	newToken := acquireHTTPToken(t, srv.URL)
	if newToken == oldToken {
		t.Fatal("token did not rotate")
	}
	if got := post(newToken); got != 200 {
		t.Fatalf("refreshed token status=%d", got)
	}
	if got := len(svc.Snapshot().ControllerSession.Messages) - before; got != 1 {
		t.Fatalf("operation executed %d times", got)
	}
}

func TestHTTPListenAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr    string
		allowed bool
	}{
		{"127.0.0.1:0", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"[::]:8080", false},
		{"192.0.2.1:8080", false},
		{"example.com:8080", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.addr), func(t *testing.T) {
			t.Parallel()
			if err := prdash.ValidateHTTPListenAddress(tc.addr); (err == nil) != tc.allowed {
				t.Fatalf("ValidateHTTPListenAddress(%q)=%v", tc.addr, err)
			}
		})
	}
}

func TestHTTPRejectsNonLoopbackPeer(t *testing.T) {
	t.Parallel()
	handler := prdash.NewHandler(newSeededService(t))
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/auth", nil)
	req.Header.Set("X-Prdash-Client", "dashboard")
	req.RemoteAddr = "192.0.2.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want forbidden", recorder.Code)
	}
}

func TestHTTPRejectsMalformedOptionalBodies(t *testing.T) {
	t.Parallel()
	cases := []string{"/api/sync", "/api/sessions/sess-alpha/ack", "/api/prs/101/request-review", "/api/prs/101/address-review", "/api/notifications/read"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(prdash.NewHandler(newSeededService(t)))
			defer srv.Close()
			token := acquireHTTPToken(t, srv.URL)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(`{`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
}
