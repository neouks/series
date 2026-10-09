package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func entryGate(t *testing.T, password string) (*httpEntryAuth, *string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	stored := string(hash)
	g, err := newHTTPEntryAuth(func(context.Context) (httpEntryConfig, error) {
		return httpEntryConfig{Enabled: true, Username: "SERIES", PasswordHash: stored, Revision: "test"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, &stored
}
func entryRequest(h http.Handler, path, password, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = ip + ":1234"
	if password != "" {
		r.SetBasicAuth("SERIES", password)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var entryOK = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

func TestHTTPEntryAllRoutesAndTwoLayers(t *testing.T) {
	g, _ := entryGate(t, "门口密码:Test123")
	s := &Server{httpAuth: g, jwtKey: []byte("test-key")}
	h := s.Handler()
	requestNumber := 0
	for _, path := range []string{"/", "/login/", "/setup/", "/_next/static/app.js", "/favicon.ico", "/api/auth/status", "/api/auth/init", "/api/health", "/api/workspace/download?path=x", "/api/tasks/1/events", "/missing"} {
		for _, method := range []string{"GET", "POST", "OPTIONS", "HEAD"} {
			t.Run(method+path, func(t *testing.T) {
				r := httptest.NewRequest(method, path, nil)
				requestNumber++
				r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestNumber)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 || w.Header().Get("WWW-Authenticate") != httpAuthChallenge || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("gate: %d %v", w.Code, w.Header())
				}
				if strings.Contains(w.Body.String(), "<html") {
					t.Fatal("leaked page")
				}
			})
		}
	}
	jwt, _ := signJWT(s.jwtKey)
	for _, tc := range []struct {
		basic, jwt bool
		want       int
	}{{false, true, 401}, {true, false, 401}, {true, true, 204}} {
		r := httptest.NewRequest("GET", "/api/private", nil)
		if tc.basic {
			r.SetBasicAuth("SERIES", "门口密码:Test123")
		}
		if tc.jwt {
			r.Header.Set("X-Series-Token", jwt)
		}
		w := httptest.NewRecorder()
		g.wrap(s.requireAuth(entryOK)).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("layers %+v: %d", tc, w.Code)
		}
	}
	if w := entryRequest(h, "/api/health", "门口密码:Test123", "192.0.2.50"); w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("health: %d %v", w.Code, w.Header())
	}
	g.loadConfig = func(context.Context) (httpEntryConfig, error) { return httpEntryConfig{}, nil }
	if w := entryRequest(s.Handler(), "/api/health", "", "192.0.2.51"); w.Code != 200 {
		t.Fatalf("disabled: %d", w.Code)
	}
}

func TestHTTPEntryLiveHashCacheAndFailure(t *testing.T) {
	g, stored := entryGate(t, "old-password")
	var comparisons atomic.Int32
	g.compare = func(a, b []byte) error { comparisons.Add(1); return bcrypt.CompareHashAndPassword(a, b) }
	h := g.wrap(entryOK)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := entryRequest(h, "/", "old-password", "192.0.2.1"); w.Code != 204 {
				t.Errorf("parallel: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if comparisons.Load() != 1 {
		t.Fatalf("comparisons=%d", comparisons.Load())
	}
	t.Logf("64 parallel resource requests: %s, bcrypt checks=%d", time.Since(start), comparisons.Load())
	newHash, _ := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	*stored = string(newHash)
	if w := entryRequest(h, "/", "old-password", "192.0.2.1"); w.Code != 401 {
		t.Fatal("old credential survived password reset")
	}
	if w := entryRequest(h, "/", "new-password", "192.0.2.1"); w.Code != 204 {
		t.Fatal("new credential rejected")
	}
	g.loadConfig = func(context.Context) (httpEntryConfig, error) {
		return httpEntryConfig{}, errors.New("database failure")
	}
	if w := entryRequest(h, "/", "new-password", "192.0.2.1"); w.Code != 503 {
		t.Fatal("cache bypassed DB failure")
	}
	for _, v := range []string{"", "broken hash"} {
		g.loadConfig = func(context.Context) (httpEntryConfig, error) {
			return httpEntryConfig{Enabled: true, Username: "SERIES", PasswordHash: v}, nil
		}
		if w := entryRequest(h, "/", "new-password", "192.0.2.1"); w.Code != 503 {
			t.Fatalf("missing/invalid hash: %d", w.Code)
		}
	}
}

func TestHTTPEntryLimitsExpiryAndUntrustedForwarding(t *testing.T) {
	g, _ := entryGate(t, "test-password")
	clock := time.Now()
	g.now = func() time.Time { return clock }
	h := g.wrap(entryOK)
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", string(rune('a'+i)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 401
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
		if want == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry")
		}
	}
	clock = clock.Add(time.Minute)
	if w := entryRequest(h, "/", "test-password", "192.0.2.1"); w.Code != 204 {
		t.Fatal("limit did not expire")
	}
	var key [32]byte
	for i := 0; i < 129; i++ {
		key[0] = byte(i)
		g.remember(key)
	}
	if len(g.success) != 128 {
		t.Fatalf("cache size %d", len(g.success))
	}
	clock = clock.Add(5 * time.Minute)
	if g.cached(key) {
		t.Fatal("cache did not expire")
	}
	for i := 0; i < 1024; i++ {
		if _, ok := g.reserve(string(rune(i))); !ok {
			t.Fatal("capacity premature")
		}
	}
	if _, ok := g.reserve("overflow"); ok {
		t.Fatal("unbounded failures map")
	}
	clock = clock.Add(time.Minute)
	if _, ok := g.reserve("overflow"); !ok {
		t.Fatal("expired windows not collected")
	}
}

func TestHTTPEntryBcryptConcurrency(t *testing.T) {
	g, _ := entryGate(t, "test-password")
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	g.compare = func([]byte, []byte) error {
		entered <- struct{}{}
		<-release
		return bcrypt.ErrMismatchedHashAndPassword
	}
	h := g.wrap(entryOK)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); entryRequest(h, "/", strings.Repeat("x", i+1), "192.0.2.1") }(i)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("check did not start")
		}
	}
	if w := entryRequest(h, "/", "different", "192.0.2.1"); w.Code != 429 {
		t.Errorf("concurrency exceeded: %d", w.Code)
	}
	close(release)
	wg.Wait()
}

func TestHTTPEntryMalformedCredentials(t *testing.T) {
	g, _ := entryGate(t, "test-password")
	h := g.wrap(entryOK)
	for _, header := range []string{"Basic !bad!", "Bearer token", "Basic", "basic Og=="} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("malformed: %d", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("SERIES", "test-password")
	r.Header.Add("Authorization", "Basic duplicate")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate auth accepted")
	}
}

func TestJWTHeaderPrecedence(t *testing.T) {
	for _, tc := range []struct {
		bearer, custom, cookie, query, want string
		hasCustom                           bool
	}{
		{"bad", "good", "cookie", "query", "bad", true},
		{"", "bad", "cookie", "query", "bad", true},
		{"", "", "cookie", "query", "", true},
		{"", "", "cookie", "query", "cookie", false},
		{"", "", "", "query", "query", false},
	} {
		r := httptest.NewRequest("GET", "/?token="+tc.query, nil)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		} else {
			r.SetBasicAuth("SERIES", "test-password")
		}
		if tc.hasCustom {
			r.Header.Set("X-Series-Token", tc.custom)
		}
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "series_token", Value: tc.cookie})
		}
		if got := extractToken(r); got != tc.want {
			t.Errorf("token=%q want=%q", got, tc.want)
		}
	}
}

func TestHTTPEntryStaticAssetsAreNotPubliclyCacheable(t *testing.T) {
	g, _ := entryGate(t, "test-password")
	files := fstest.MapFS{"_next/static/app.js": {Data: []byte("protected")}}
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveFileIfExists(w, r, files, "_next/static/app.js") }))
	w := entryRequest(h, "/_next/static/app.js", "test-password", "192.0.2.1")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("protected cache: %d %v", w.Code, w.Header())
	}
}
