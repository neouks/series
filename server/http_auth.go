package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/singleflight"
)

const httpAuthChallenge = `Basic realm="SERIES", charset="UTF-8"`

type httpEntryContextKey struct{}

type authFailureWindow struct {
	until time.Time
	used  int // failed responses within this fixed window
}

type httpEntryAuth struct {
	loadConfig func(context.Context) (httpEntryConfig, error)
	bridge     func(*http.Request, httpEntryConfig) bool
	compare    func([]byte, []byte) error
	now        func() time.Time
	key        [32]byte
	mu         sync.Mutex
	success    map[[32]byte]time.Time
	failures   map[string]*authFailureWindow
	checks     chan struct{}
	flight     singleflight.Group
}

func newHTTPEntryAuth(load func(context.Context) (httpEntryConfig, error)) (*httpEntryAuth, error) {
	g := &httpEntryAuth{
		loadConfig: load, compare: bcrypt.CompareHashAndPassword, now: time.Now,
		success: make(map[[32]byte]time.Time), failures: make(map[string]*authFailureWindow),
		checks: make(chan struct{}, 4),
	}
	_, err := rand.Read(g.key[:])
	return g, err
}

func (g *httpEntryAuth) digest(hash, user, password string) [32]byte {
	h := hmac.New(sha256.New, g.key[:])
	for _, value := range []string{hash, user, password} {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func (g *httpEntryAuth) cached(key [32]byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Before(g.success[key])
}

func (g *httpEntryAuth) remember(key [32]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, expiry := range g.success {
		if !now.Before(expiry) {
			delete(g.success, k)
		}
	}
	if len(g.success) >= 128 {
		var oldest [32]byte
		var expiry time.Time
		for k, v := range g.success {
			if expiry.IsZero() || v.Before(expiry) {
				oldest, expiry = k, v
			}
		}
		delete(g.success, oldest)
	}
	g.success[key] = now.Add(5 * time.Minute)
}

func (g *httpEntryAuth) reserve(ip string) (*authFailureWindow, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, window := range g.failures {
		if !now.Before(window.until) {
			delete(g.failures, k)
		}
	}
	w := g.failures[ip]
	if w == nil {
		if len(g.failures) >= 1024 {
			return nil, false
		}
		w = &authFailureWindow{until: now.Add(time.Minute)}
		g.failures[ip] = w
	}
	if w.used >= 10 {
		return nil, false
	}
	return w, true
}

func (g *httpEntryAuth) recordFailure(w *authFailureWindow) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.used >= 10 {
		return http.StatusTooManyRequests
	}
	w.used++
	return http.StatusUnauthorized
}

func entryAuthError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", httpAuthChallenge)
		writeErr(w, status, "HTTP 入口认证失败")
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "60")
		writeErr(w, status, "认证请求过于频繁，请稍后重试")
	default:
		writeErr(w, status, "入口认证暂不可用，请联系管理员")
	}
}

func (g *httpEntryAuth) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read live settings even on cache hits: saved changes apply immediately,
		// and a database outage must never fail open.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		cfg, err := g.loadConfig(ctx)
		cancel()
		if err != nil {
			entryAuthError(w, http.StatusServiceUnavailable)
			return
		}
		if !cfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		hash := cfg.PasswordHash
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			entryAuthError(w, http.StatusServiceUnavailable)
			return
		}
		if g.bridge != nil && g.bridge(r, cfg) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpEntryContextKey{}, true)))
			return
		}
		user, password, ok := r.BasicAuth()
		validFormat := ok && user == cfg.Username && len(password) > 0 && len(password) <= 72 &&
			!strings.ContainsFunc(password, unicode.IsControl) && len(r.Header.Values("Authorization")) == 1
		key := g.digest(hash, user, password)
		if validFormat && g.cached(key) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpEntryContextKey{}, true)))
			return
		}
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		window, allowed := g.reserve(ip)
		if !allowed {
			entryAuthError(w, http.StatusTooManyRequests)
			return
		}
		if !validFormat {
			entryAuthError(w, g.recordFailure(window))
			return
		}
		// One expensive comparison per distinct credential, even when a browser
		// fetches many assets at once. The key never contains the raw password.
		result, _, _ := g.flight.Do(string(key[:]), func() (any, error) {
			if g.cached(key) {
				return http.StatusOK, nil
			}
			select {
			case g.checks <- struct{}{}:
				defer func() { <-g.checks }()
			default:
				return http.StatusTooManyRequests, nil
			}
			if g.compare([]byte(hash), []byte(password)) != nil {
				return http.StatusUnauthorized, nil
			}
			g.remember(key)
			return http.StatusOK, nil
		})
		status := result.(int)
		if status == http.StatusUnauthorized {
			status = g.recordFailure(window)
		}
		if status != http.StatusOK {
			entryAuthError(w, status)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpEntryContextKey{}, true)))
	})
}
