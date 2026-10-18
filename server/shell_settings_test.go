package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShellSettingsReadonlyAndLegacyRejection(t *testing.T) {
	s, _ := modeServer(t)
	if err := s.m.pg.SetSetting("shell_mode", "cmd"); err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM settings WHERE key='shell_mode'`)
	before := httptest.NewRecorder()
	s.getSettings(before, httptest.NewRequest("GET", "/api/settings", nil))
	var values map[string]any
	if err := json.Unmarshal(before.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if _, ok := values["shell_mode"]; ok {
		t.Fatal("legacy override still exposed")
	}
	if detected, ok := values["shell_detected"].(map[string]any); !ok || detected["mode"] != defaultShellMode() {
		t.Fatalf("detection=%v", values["shell_detected"])
	}
	for _, legacy := range []string{`"cmd"`, `null`, `42`} {
		out := httptest.NewRecorder()
		s.putSettings(out, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"shell_mode":`+legacy+`,"global_proxy":"http://legacy-invalid.test:1234"}`)))
		if out.Code != 400 || !strings.Contains(out.Body.String(), "shell_mode") {
			t.Fatalf("%d %s", out.Code, out.Body.String())
		}
		if s.m.GlobalProxy() == "http://legacy-invalid.test:1234" {
			t.Fatal("partially saved invalid request")
		}
	}
	stored, _, err := s.m.pg.GetSetting("shell_mode")
	if err != nil || stored != "cmd" {
		t.Fatalf("old data changed: %q %v", stored, err)
	}
}
