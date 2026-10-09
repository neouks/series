package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const httpEntrySetting = "auth.http_entry"
const httpEntryCookie = "series_http_entry"

type httpEntryConfig struct {
	Enabled      bool   `json:"enabled"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Revision     string `json:"revision"`
}

type entrySettingReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadHTTPEntryConfig(ctx context.Context, reader entrySettingReader) (httpEntryConfig, error) {
	cfg := httpEntryConfig{Username: "entry"}
	var raw string
	err := reader.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, httpEntrySetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	if !validEntryUsername(cfg.Username) {
		return cfg, errors.New("invalid entry username")
	}
	if cfg.PasswordHash != "" {
		if _, err = bcrypt.Cost([]byte(cfg.PasswordHash)); err != nil {
			return cfg, err
		}
	}
	if cfg.Enabled && (cfg.PasswordHash == "" || cfg.Revision == "") {
		return cfg, errors.New("incomplete entry credentials")
	}
	return cfg, nil
}

func validEntryUsername(v string) bool {
	return v != "" && len(v) <= 128 && utf8.ValidString(v) && !strings.Contains(v, ":") && !strings.ContainsFunc(v, unicode.IsControl)
}

func (s *Server) initHTTPAuth() {
	s.httpAuthOnce.Do(func() {
		if s.httpAuth != nil {
			return
		}
		s.httpAuth, s.httpAuthErr = newHTTPEntryAuth(func(ctx context.Context) (httpEntryConfig, error) {
			if s.m == nil || s.m.pg == nil {
				return httpEntryConfig{}, errors.New("database unavailable")
			}
			return loadHTTPEntryConfig(ctx, s.m.pg)
		})
		if s.httpAuthErr == nil {
			s.httpAuth.bridge = s.validHTTPEntryBridge
		}
	})
}

func (s *Server) entryBridgeValue(cfg httpEntryConfig, token string) string {
	mac := hmac.New(sha256.New, s.jwtKey)
	mac.Write([]byte("series-http-entry\x00" + cfg.Revision + "\x00"))
	sum := sha256.Sum256([]byte(token))
	mac.Write(sum[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Only the browser performing an authenticated save receives this HttpOnly
// grant. It is bound to both the exact live JWT and the current config revision.
// Logging out, changing JWT, editing settings, or expiry invalidates it.
func (s *Server) validHTTPEntryBridge(r *http.Request, cfg httpEntryConfig) bool {
	cookie, err := r.Cookie(httpEntryCookie)
	if err != nil {
		return false
	}
	token := extractToken(r)
	if !verifyJWT(token, s.jwtKey) {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(s.entryBridgeValue(cfg, token)))
}

func httpEntryPublic(cfg httpEntryConfig) map[string]any {
	return map[string]any{"enabled": cfg.Enabled, "username": cfg.Username, "password_set": cfg.PasswordHash != ""}
}

func (s *Server) getHTTPAuthSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cfg, err := loadHTTPEntryConfig(r.Context(), s.m.pg)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, httpEntryPublic(cfg))
}

func (s *Server) putHTTPAuthSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Enabled  *bool  `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.Enabled == nil {
		writeErr(w, 400, "访问验证配置格式错误")
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeErr(w, 400, "访问验证配置格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !validEntryUsername(req.Username) {
		writeErr(w, 400, "验证用户名不能为空、不能包含冒号或控制字符，且最多 128 字节")
		return
	}
	var passwordHash string
	if req.Password != "" {
		if msg := validatePassword(req.Password); msg != "" {
			writeErr(w, 400, msg)
			return
		}
		if !utf8.ValidString(req.Password) || strings.ContainsFunc(req.Password, unicode.IsControl) {
			writeErr(w, 400, "验证密码不能包含控制字符")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeErr(w, 500, "验证密码保存失败")
			return
		}
		passwordHash = string(hash)
	}
	tx, err := s.m.pg.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	defer tx.Rollback()
	// Also serialize the first insert across multiple backend processes.
	if _, err = tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(7337741091)`); err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	cfg, err := loadHTTPEntryConfig(r.Context(), tx)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	cfg.Enabled = *req.Enabled
	cfg.Username = req.Username
	if passwordHash != "" {
		cfg.PasswordHash = passwordHash
	}
	if cfg.Enabled && cfg.PasswordHash == "" {
		writeErr(w, 400, "首次启用访问验证需要设置验证密码")
		return
	}
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		writeErr(w, 500, "验证配置保存失败")
		return
	}
	cfg.Revision = base64.RawURLEncoding.EncodeToString(nonce[:])
	raw, _ := json.Marshal(cfg)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, httpEntrySetting, string(raw)); err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if err = tx.Commit(); err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	cookie := &http.Cookie{Name: httpEntryCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")}
	if cfg.Enabled {
		cookie.Value = s.entryBridgeValue(cfg, extractToken(r))
		cookie.MaxAge = int(jwtTTL.Seconds())
	} else {
		cookie.MaxAge = -1
	}
	http.SetCookie(w, cookie)
	writeJSON(w, 200, httpEntryPublic(cfg))
}
