package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBodySize       = 100 << 20 // 100MB
	shutdownTimeout   = 30 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 120 * time.Second
	idleTimeout       = 60 * time.Second
	dialTimeout       = 10 * time.Second
	tlsHandshake      = 5 * time.Second
	respHeaderTimeout = 0 // no limit — LLM responses can be slow
	rateLimitPerSec   = 100
	rateLimitBurst    = 20
)

// hop-by-hop headers that must not be forwarded (RFC 7230 Section 6.1)
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

var logger *slog.Logger

func init() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validateURL checks that a URL string is valid and uses http/https scheme.
func validateURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid scheme %q: only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in URL: %s", rawURL)
	}
	return u, nil
}

// sanitizeURL removes credentials from a URL for safe logging.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

// extractModel parses the model field from a JSON body.
func extractModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
}

// isAnthropicModel checks if the model should be routed to Anthropic (case-insensitive).
func isAnthropicModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "claude-") ||
		strings.HasPrefix(m, "anthropic/")
}

// requestID generates a simple unique request ID from timestamp + counter.
var reqCounter struct {
	sync.Mutex
	n uint64
}

func nextRequestID() string {
	reqCounter.Lock()
	reqCounter.n++
	id := reqCounter.n
	reqCounter.Unlock()
	return fmt.Sprintf("%d-%04d", time.Now().UnixMilli(), id)
}

// rateLimiter implements a simple token bucket rate limiter (stdlib only).
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	rate     float64 // tokens per second
	lastTime time.Time
}

func newRateLimiter(ratePerSec float64, burst int) *rateLimiter {
	return &rateLimiter{
		tokens:   float64(burst),
		max:      float64(burst),
		rate:     ratePerSec,
		lastTime: time.Now(),
	}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.lastTime = now

	rl.tokens += elapsed * rl.rate
	if rl.tokens > rl.max {
		rl.tokens = rl.max
	}

	if rl.tokens < 1 {
		return false
	}
	rl.tokens--
	return true
}

// metrics tracks basic request counters (in-memory, stdlib only).
type metrics struct {
	mu                sync.Mutex
	totalRequests     uint64
	anthropicRequests uint64
	downstreamReqs    uint64
	errors            uint64
}

var stats metrics

func (m *metrics) incTotal()      { m.mu.Lock(); m.totalRequests++; m.mu.Unlock() }
func (m *metrics) incAnthropic()  { m.mu.Lock(); m.anthropicRequests++; m.mu.Unlock() }
func (m *metrics) incDownstream() { m.mu.Lock(); m.downstreamReqs++; m.mu.Unlock() }
func (m *metrics) incError()      { m.mu.Lock(); m.errors++; m.mu.Unlock() }
func (m *metrics) snapshot() map[string]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]uint64{
		"total_requests":     m.totalRequests,
		"anthropic_requests": m.anthropicRequests,
		"downstream_requests": m.downstreamReqs,
		"errors":             m.errors,
	}
}

// proxyTransport is a shared transport with proper timeouts.
var proxyTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout: dialTimeout,
	}).DialContext,
	TLSHandshakeTimeout:   tlsHandshake,
	ResponseHeaderTimeout: respHeaderTimeout,
	IdleConnTimeout:       idleTimeout,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
}

func proxyTo(w http.ResponseWriter, r *http.Request, body []byte, target *url.URL, extraHeaders map[string]string) {
	proxy := &httputil.ReverseProxy{
		Transport: proxyTransport,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = r.URL.Path
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host

			// Clear and copy headers, filtering hop-by-hop
			for k := range req.Header {
				delete(req.Header, k)
			}
			for k, v := range r.Header {
				if !hopByHopHeaders[strings.ToLower(k)] {
					req.Header[k] = v
				}
			}
			for k, v := range extraHeaders {
				req.Header.Set(k, v)
			}

			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			reqID := req.Header.Get("X-Request-ID")
			logger.Error("proxy error", "request_id", reqID, "error", err.Error())
			stats.incError()
			http.Error(rw, "upstream error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

type proxyHandler struct {
	downstreamURL *url.URL
	anthropicURL  *url.URL
	limiter       *rateLimiter
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	// Metrics endpoint
	if r.URL.Path == "/metrics" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats.snapshot())
		return
	}

	// Method validation
	if r.Method != http.MethodPost && r.Method != http.MethodGet &&
		r.Method != http.MethodPut && r.Method != http.MethodPatch &&
		r.Method != http.MethodDelete && r.Method != http.MethodOptions {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// CORS preflight
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, Anthropic-Version")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Rate limiting
	if !h.limiter.allow() {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Request ID
	reqID := nextRequestID()
	w.Header().Set("X-Request-ID", reqID)

	stats.incTotal()

	// Read body with size limit
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		stats.incError()
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	model := extractModel(body)

	if isAnthropicModel(model) {
		logger.Info("routing request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"target", "anthropic",
		)
		stats.incAnthropic()
		r.Header.Set("X-Request-ID", reqID)
		proxyTo(w, r, body, h.anthropicURL, nil)
	} else {
		logger.Info("routing request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"target", "downstream",
		)
		stats.incDownstream()
		r.Header.Set("X-Request-ID", reqID)
		proxyTo(w, r, body, h.downstreamURL, map[string]string{
			"Content-Type": "application/json",
		})
	}
}

func run() error {
	downstreamRaw := getEnv("DOWNSTREAM_URL", "http://localhost:8080")
	listenAddr := getEnv("PROXY_ADDR", ":4000")

	downstreamTarget, err := validateURL(downstreamRaw)
	if err != nil {
		return fmt.Errorf("DOWNSTREAM_URL: %w", err)
	}

	anthropicTarget, err := validateURL("https://api.anthropic.com")
	if err != nil {
		return fmt.Errorf("anthropic URL: %w", err)
	}

	if _, err := net.ResolveTCPAddr("tcp", listenAddr); err != nil {
		return fmt.Errorf("PROXY_ADDR %q: %w", listenAddr, err)
	}

	handler := &proxyHandler{
		downstreamURL: downstreamTarget,
		anthropicURL:  anthropicTarget,
		limiter:       newRateLimiter(rateLimitPerSec, rateLimitBurst),
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	logger.Info("llm-proxy starting",
		"addr", listenAddr,
		"downstream", sanitizeURL(downstreamRaw),
		"anthropic", "https://api.anthropic.com",
	)

	// Graceful shutdown
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return server.Shutdown(ctx)
	case err := <-errCh:
		return err
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
