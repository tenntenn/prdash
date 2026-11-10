package prdash

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// ValidateHTTPListenAddress restricts the dashboard to loopback listeners.
// Remote serving requires a separately designed authentication boundary.
func ValidateHTTPListenAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid HTTP listen address: %w", err)
	}
	if !isLoopbackHost(host) {
		return errors.New("HTTP listener must use localhost or a loopback IP address")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func allowedHTTPHost(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		// A Host without a port is valid for HTTP's default port.
		if strings.ContainsAny(authority, "[]:") {
			return false
		}
		host = authority
	}
	return isLoopbackHost(host)
}

func sameHTTPOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	origin, err := url.Parse(values[0])
	if err != nil || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host)
}

func protectLocalHTTP(next http.Handler) http.Handler {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Errorf("create HTTP operation token: %w", err))
	}
	token := hex.EncodeToString(entropy[:])
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !allowedHTTPHost(r.Host) || !sameHTTPOrigin(r) {
			writeError(w, http.StatusForbidden, errors.New("disallowed HTTP host or origin"))
			return
		}
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopbackHost(remote) {
			writeError(w, http.StatusForbidden, errors.New("HTTP access is restricted to loopback clients"))
			return
		}
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		bootstrap := r.URL.Path == "/api/auth"
		if mutation || bootstrap {
			site := r.Header.Get("Sec-Fetch-Site")
			if site != "" && site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, errors.New("cross-origin HTTP operation rejected"))
				return
			}
			w.Header().Set("Cache-Control", "no-store")
		}
		if bootstrap {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeError(w, http.StatusMethodNotAllowed, errors.New("token acquisition requires GET"))
				return
			}
			if r.Header.Get("X-Prdash-Client") != "dashboard" {
				writeError(w, http.StatusForbidden, errors.New("token acquisition requires X-Prdash-Client: dashboard"))
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"token": token})
			return
		}
		if mutation {
			supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="prdash"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operation token missing or expired", "code": "authentication_required"})
				return
			}
			mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if parseErr != nil || mediaType != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}
