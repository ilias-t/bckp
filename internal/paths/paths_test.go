package paths

import (
	"strings"
	"testing"
)

func TestForHome(t *testing.T) {
	u := ForHome("/Users/test")
	want := map[string]string{
		"EngineSocket":   "/Users/test/Library/Application Support/backpack/engine.sock",
		"EngineLock":     "/Users/test/Library/Application Support/backpack/engine.lock",
		"RestoreJournal": "/Users/test/Library/Application Support/backpack/restore.json",
		"EngineLog":      "/Users/test/Library/Logs/backpack/engine.log",
		"ConfigFile":     "/Users/test/.config/backpack/config.toml",
	}
	got := map[string]string{
		"EngineSocket":   u.EngineSocket,
		"EngineLock":     u.EngineLock,
		"RestoreJournal": u.RestoreJournal,
		"EngineLog":      u.EngineLog,
		"ConfigFile":     u.ConfigFile,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestCheckSocketPath(t *testing.T) {
	if err := CheckSocketPath(DaemonSocket); err != nil {
		t.Fatalf("daemon socket: %v", err)
	}
	if err := CheckSocketPath(ForHome("/Users/someone").EngineSocket); err != nil {
		t.Fatalf("engine socket: %v", err)
	}
	if err := CheckSocketPath("/" + strings.Repeat("x", MaxSocketPathLen)); err == nil {
		t.Fatal("expected error for overlong path")
	}
}
