package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGetEnv(t *testing.T) {
	t.Setenv("TEST_GET_ENV_KEY", "custom")
	if got := getEnv("TEST_GET_ENV_KEY", "default"); got != "custom" {
		t.Errorf("getEnv = %q, want %q", got, "custom")
	}
	if got := getEnv("TEST_GET_ENV_MISSING", "fallback"); got != "fallback" {
		t.Errorf("getEnv = %q, want %q", got, "fallback")
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid http", "http://localhost:8080", false},
		{"valid https", "https://api.anthropic.com", false},
		{"missing scheme", "localhost:8080", true},
		{"file scheme", "file:///etc/passwd", true},
		{"gopher scheme", "gopher://evil.com", true},
		{"empty host", "http://", true},
		{"ftp scheme", "ftp://files.example.com", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateURL(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateURL(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestSanitizeURL(t *testing.T) {
	got := sanitizeURL("http://admin:secret@example.com/path")
	if strings.Contains(got, "secret") {
		t.Errorf("sanitizeURL should remove password, got %q", got)
	}
	if !strings.Contains(got, "admin") {
		// user info should no longer contain original credentials
		t.Logf("sanitizeURL masked credentials: %q", got)
	}

	got = sanitizeURL("http://example.com/path")
	if got != "http://example.com/path" {
		t.Errorf("sanitizeURL = %q, want %q", got, "http://example.com/path")
	}

	got = sanitizeURL("://invalid")
	if got != "<invalid-url>" {
		t.Errorf("sanitizeURL invalid = %q, want %q", got, "<invalid-url>")
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		model string
	}{
		{"claude model", `{"model":"claude-sonnet-4-6","messages":[]}`, "claude-sonnet-4-6"},
		{"ollama model", `{"model":"qwen2.5-coder:32b"}`, "qwen2.5-coder:32b"},
		{"empty body", `{}`, ""},
		{"no model field", `{"messages":[]}`, ""},
		{"invalid json", `not json`, ""},
		{"empty string", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractModel([]byte(tc.body))
			if got != tc.model {
				t.Errorf("extractModel = %q, want %q", got, tc.model)
			}
		})
	}
}

func TestIsAnthropicModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-4-6", true},
		{"claude-opus-4-6", true},
		{"claude-haiku-4-5-20251001", true},
		{"anthropic/claude-3", true},
		{"CLAUDE-sonnet-4-6", true},   // case insensitive
		{"Claude-opus", true},          // mixed case
		{"ANTHROPIC/model", true},      // uppercase prefix
		{"qwen2.5-coder:32b", false},
		{"openai/gpt-4o", false},
		{"llama3:70b", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			got := isAnthropicModel(tc.model)
			if got != tc.want {
				t.Errorf("isAnthropicModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestNextRequestID(t *testing.T) {
	id1 := nextRequestID()
	id2 := nextRequestID()
	if id1 == id2 {
		t.Error("nextRequestID should return unique IDs")
	}
	if id1 == "" || id2 == "" {
		t.Error("nextRequestID should not return empty strings")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(10, 3)

	// Should allow burst
	for i := 0; i < 3; i++ {
		if !rl.allow() {
			t.Errorf("request %d should be allowed within burst", i)
		}
	}

	// Should deny after burst
	if rl.allow() {
		t.Error("request should be denied after burst exhausted")
	}

	// Should recover after waiting
	time.Sleep(150 * time.Millisecond) // ~1.5 tokens at 10/sec
	if !rl.allow() {
		t.Error("request should be allowed after token recovery")
	}
}

func TestMetrics(t *testing.T) {
	var m metrics
	m.incTotal()
	m.incTotal()
	m.incAnthropic()
	m.incDownstream()
	m.incError()

	snap := m.snapshot()
	if snap["total_requests"] != 2 {
		t.Errorf("total = %d, want 2", snap["total_requests"])
	}
	if snap["anthropic_requests"] != 1 {
		t.Errorf("anthropic = %d, want 1", snap["anthropic_requests"])
	}
	if snap["downstream_requests"] != 1 {
		t.Errorf("downstream = %d, want 1", snap["downstream_requests"])
	}
	if snap["errors"] != 1 {
		t.Errorf("errors = %d, want 1", snap["errors"])
	}
}

func newTestHandler(t *testing.T, backend *httptest.Server) *proxyHandler {
	t.Helper()
	downstream, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := url.Parse(backend.URL) // use same backend for tests
	if err != nil {
		t.Fatal(err)
	}
	return &proxyHandler{
		downstreamURL: downstream,
		anthropicURL:  anthropic,
		limiter:       newRateLimiter(1000, 100),
	}
}

func TestHealthEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("health body = %v, want status=ok", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("metrics status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]uint64
	json.NewDecoder(rec.Body).Decode(&body)
	if _, ok := body["total_requests"]; !ok {
		t.Error("metrics response should contain total_requests")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)

	for _, method := range []string{"TRACE", "CONNECT"} {
		req := httptest.NewRequest(method, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestOptionsPreflightCORS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	req := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("OPTIONS should include POST in Allow-Methods")
	}
}

func TestRateLimitResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	handler.limiter = newRateLimiter(0, 0) // deny all

	body := `{"model":"qwen2.5-coder:32b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("rate limit status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
}

func TestRequestIDHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	body := `{"model":"qwen2.5-coder:32b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("response should contain X-Request-ID header")
	}
}

func TestBodyTooLarge(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, backend)

	// Create a body that exceeds maxBodySize
	largeBody := make([]byte, maxBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(largeBody))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestAnthropicRouting(t *testing.T) {
	var receivedPath string
	var receivedBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "sk-ant-xxx")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("anthropic routing status = %d, want %d", rec.Code, http.StatusOK)
	}
	if receivedPath != "/v1/messages" {
		t.Errorf("path = %q, want %q", receivedPath, "/v1/messages")
	}
	if receivedBody != body {
		t.Errorf("body not forwarded correctly")
	}
}

func TestDownstreamRouting(t *testing.T) {
	var receivedContentType string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	body := `{"model":"qwen2.5-coder:32b","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("downstream routing status = %d, want %d", rec.Code, http.StatusOK)
	}
	if receivedContentType != "application/json" {
		t.Errorf("content-type = %q, want %q", receivedContentType, "application/json")
	}
}

func TestHopByHopHeadersFiltered(t *testing.T) {
	var receivedHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	body := `{"model":"qwen2.5-coder:32b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	req.Header.Set("X-Custom-Header", "should-pass")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if receivedHeaders.Get("Connection") != "" {
		t.Error("Connection header should be filtered")
	}
	if receivedHeaders.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding header should be filtered")
	}
	if receivedHeaders.Get("Proxy-Authorization") != "" {
		t.Error("Proxy-Authorization header should be filtered")
	}
	if receivedHeaders.Get("X-Custom-Header") != "should-pass" {
		t.Error("Custom headers should be forwarded")
	}
}

func TestEmptyModelRoutesToDownstream(t *testing.T) {
	var routed bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler := newTestHandler(t, backend)
	body := `{"messages":[]}` // no model field
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !routed {
		t.Error("empty model should route to downstream")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxyErrorHandler(t *testing.T) {
	// Use a URL that will fail to connect
	badURL, _ := url.Parse("http://127.0.0.1:1") // port 1 — nothing listens here
	handler := &proxyHandler{
		downstreamURL: badURL,
		anthropicURL:  badURL,
		limiter:       newRateLimiter(1000, 100),
	}

	body := `{"model":"qwen2.5-coder:32b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("proxy error status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(rec.Body.String(), "upstream error") {
		t.Error("proxy error should return generic 'upstream error' message")
	}
}
