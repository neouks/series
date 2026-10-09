package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestHTTPEntryRuntimeSettings(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	old, exists, err := m.pg.GetSetting(httpEntrySetting)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if exists {
			m.pg.SetSetting(httpEntrySetting, old)
		} else {
			m.pg.Exec(`DELETE FROM settings WHERE key=$1`, httpEntrySetting)
		}
	}()
	m.pg.Exec(`DELETE FROM settings WHERE key=$1`, httpEntrySetting)
	s := &Server{m: m, jwtKey: []byte("isolated-http-settings-key")}
	h := s.Handler()
	token, _ := signJWT(s.jwtKey)
	request := func(method, path, body, user, password, jwt string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if user != "" {
			r.SetBasicAuth(user, password)
		}
		if jwt != "" {
			r.Header.Set("X-Series-Token", jwt)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		// Avoid sharing failed-request limits between unrelated assertions.
		r.RemoteAddr = "192.0.2." + string(rune('a'+len(path))) + ":1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	initial := request("GET", "/api/settings/http-auth", "", "", "", token, nil)
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), `"enabled":false`) {
		t.Fatalf("default %d %s", initial.Code, initial.Body)
	}
	if w := request("PUT", "/api/settings/http-auth", `{"enabled":true,"username":"entry","password":""}`, "", "", token, nil); w.Code != 400 {
		t.Fatal("enabled without password")
	}
	for _, body := range []string{
		`{"enabled":false,"username":"entry","password":""} {}`,
		`{"enabled":true,"username":"entry","password":"short"}`,
		`{"enabled":false,"username":"bad:name","password":""}`,
		`{"username":"entry","password":""}`,
		`{"enabled":false,"username":"entry","password":"","extra":true}`,
	} {
		if w := request("PUT", "/api/settings/http-auth", body, "", "", token, nil); w.Code != 400 {
			t.Fatalf("invalid config accepted: %d", w.Code)
		}
	}
	if _, exists, _ := m.pg.GetSetting(httpEntrySetting); exists {
		t.Fatal("rejected config persisted")
	}
	save := request("PUT", "/api/settings/http-auth", `{"enabled":true,"username":"entry","password":"Entry-password-123"}`, "", "", token, nil)
	if save.Code != 200 {
		t.Fatalf("save %d %s", save.Code, save.Body)
	}
	if strings.Contains(save.Body.String(), "Entry-password") || strings.Contains(save.Body.String(), "password_hash") {
		t.Fatal("credential leaked")
	}
	cookies := save.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("missing browser continuation")
	}
	bridge := cookies[0]
	if w := request("GET", "/api/health", "", "", "", "", nil); w.Code != 401 {
		t.Fatal("enable was not immediate")
	}
	if w := request("GET", "/api/settings/http-auth", "", "", "", token, nil); w.Code != 401 {
		t.Fatal("JWT alone bypassed gate")
	}
	if w := request("GET", "/api/settings/http-auth", "", "", "", token, bridge); w.Code != 200 {
		t.Fatal("saving browser interrupted")
	}
	if w := request("GET", "/api/health", "", "", "", "", bridge); w.Code != 401 {
		t.Fatal("grant without JWT accepted")
	}
	copied := *bridge
	copied.Value += "bad"
	if w := request("GET", "/api/health", "", "", "", token, &copied); w.Code != 401 {
		t.Fatal("tampered grant accepted")
	}
	if w := request("GET", "/api/health", "", "entry", "Entry-password-123", "", nil); w.Code != 200 {
		t.Fatal("custom credentials rejected")
	}
	if w := request("GET", "/api/settings/http-auth", "", "entry", "Entry-password-123", "", nil); w.Code != 401 {
		t.Fatal("Basic replaced system login")
	}
	before, _ := loadHTTPEntryConfig(context.Background(), m.pg)
	unchanged := request("PUT", "/api/settings/http-auth", `{"enabled":true,"username":"入口","password":""}`, "", "", token, bridge)
	if unchanged.Code != 200 {
		t.Fatalf("retain password: %d", unchanged.Code)
	}
	after, _ := loadHTTPEntryConfig(context.Background(), m.pg)
	if before.PasswordHash != after.PasswordHash {
		t.Fatal("blank password changed hash")
	}
	bridge2 := unchanged.Result().Cookies()[0]
	if w := request("GET", "/api/health", "", "entry", "Entry-password-123", "", nil); w.Code != 401 {
		t.Fatal("old username still accepted")
	}
	if w := request("GET", "/api/health", "", "入口", "Entry-password-123", "", nil); w.Code != 200 {
		t.Fatal("new username rejected")
	}
	if w := request("GET", "/api/health", "", "", "", token, bridge); w.Code != 401 {
		t.Fatal("old revision grant accepted")
	}
	changed := request("PUT", "/api/settings/http-auth", `{"enabled":true,"username":"入口","password":"New-password-456"}`, "", "", token, bridge2)
	if changed.Code != 200 {
		t.Fatalf("change %d", changed.Code)
	}
	if w := request("GET", "/api/health", "", "入口", "Entry-password-123", "", nil); w.Code != 401 {
		t.Fatal("old password cached")
	}
	// A new handler/server simulates restart: the persisted configuration applies.
	restarted := &Server{m: m, jwtKey: s.jwtKey}
	restartedHandler := restarted.Handler()
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.SetBasicAuth("入口", "New-password-456")
	w := httptest.NewRecorder()
	restartedHandler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("restart lost credentials")
	}
	bridge3 := changed.Result().Cookies()[0]
	disabled := request("PUT", "/api/settings/http-auth", `{"enabled":false,"username":"入口","password":""}`, "", "", token, bridge3)
	if disabled.Code != 200 || disabled.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("disable failed")
	}
	if w := request("GET", "/api/health", "", "", "", "", nil); w.Code != 200 {
		t.Fatal("disable was not immediate")
	}
	cfg, _ := loadHTTPEntryConfig(context.Background(), m.pg)
	if bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte("New-password-456")) != nil {
		t.Fatal("disable deleted password")
	}
	reenabled := request("PUT", "/api/settings/http-auth", `{"enabled":true,"username":"入口","password":""}`, "", "", token, nil)
	if reenabled.Code != 200 {
		t.Fatal("cannot reenable saved credentials")
	}
	// Corruption and DB failure are never interpreted as disabled.
	m.pg.SetSetting(httpEntrySetting, "malformed")
	if w := request("GET", "/api/health", "", "入口", "New-password-456", "", nil); w.Code != 503 {
		t.Fatal("malformed config failed open")
	}
}

func TestHTTPEntryBrowserGrantScope(t *testing.T) {
	s := &Server{jwtKey: []byte("browser-grant-test")}
	cfg := httpEntryConfig{Enabled: true, Revision: "revision-one"}
	token, _ := signJWT(s.jwtKey)
	grant := &http.Cookie{Name: httpEntryCookie, Value: s.entryBridgeValue(cfg, token)}
	r := httptest.NewRequest("GET", "/static/app.js", nil)
	r.AddCookie(grant)
	r.AddCookie(&http.Cookie{Name: "series_token", Value: token})
	if !s.validHTTPEntryBridge(r, cfg) {
		t.Fatal("browser resource navigation lost access")
	}
	r.Header.Set("X-Series-Token", "invalid")
	if s.validHTTPEntryBridge(r, cfg) {
		t.Fatal("invalid explicit JWT fell back to cookie")
	}
	expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "SERIES", ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	}).SignedString(s.jwtKey)
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Series-Token", expired)
	r.AddCookie(&http.Cookie{Name: httpEntryCookie, Value: s.entryBridgeValue(cfg, expired)})
	if s.validHTTPEntryBridge(r, cfg) {
		t.Fatal("expired JWT extended browser grant")
	}
}

func TestHTTPEntryValidation(t *testing.T) {
	for _, v := range []string{"", "x:y", "x\n", strings.Repeat("中", 43)} {
		if validEntryUsername(v) {
			t.Fatalf("accepted invalid username %q", v)
		}
	}
	if !validEntryUsername("入口 user") {
		t.Fatal("valid UTF-8 rejected")
	}
	cfg := httpEntryConfig{Username: "entry", PasswordHash: "secret", Revision: "secret-revision"}
	b, _ := json.Marshal(httpEntryPublic(cfg))
	if strings.Contains(string(b), "secret") {
		t.Fatal("secret in public config")
	}
}
