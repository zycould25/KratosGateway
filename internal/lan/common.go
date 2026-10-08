package lan

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrShortToken  = errors.New("LAN_TOKEN must be at least 24 characters")
	ErrInvalidRole = errors.New("ROLE must be hub or node")
)

type NodeInfo struct {
	ID              string    `json:"id"`
	FileURL         string    `json:"file_url"`
	MinioURL        string    `json:"minio_url,omitempty"`
	MinioConsoleURL string    `json:"minio_console_url,omitempty"`
	PostgresAddress string    `json:"postgres_address,omitempty"`
	MinioHealthy    bool      `json:"minio_healthy"`
	PostgresHealthy bool      `json:"postgres_healthy"`
	PostgresRole    string    `json:"postgres_role"`
	LastSeen        time.Time `json:"last_seen"`
}

func authorized(r *http.Request, token string) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1
}

func requireAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !authorized(r, token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func validKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\\\x00") || strings.HasPrefix(key, "/") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".upload-") {
			return false
		}
	}
	return true
}

func newHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
}
