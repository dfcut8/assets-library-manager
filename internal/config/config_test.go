package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateGeneratesDefaultsAndIgnoresEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-be-used")
	root := t.TempDir()

	cfg, created, err := LoadOrCreate(root)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if !created {
		t.Fatal("LoadOrCreate() created = false, want true")
	}
	if cfg.Codex.Command != "codex" {
		t.Fatalf("Codex command = %q, want codex", cfg.Codex.Command)
	}
	if !cfg.Server.OpenBrowser {
		t.Fatal("OpenBrowser = false, want true")
	}

	path := filepath.Join(root, FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(config.json) error = %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadOrCreatePreservesExistingConfig(t *testing.T) {
	root := t.TempDir()
	want := Default()
	want.Server.Port = 8123
	want.Codex.Command = "custom-codex"
	writeConfig(t, root, want)
	original, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}

	got, created, err := LoadOrCreate(root)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if created {
		t.Fatal("LoadOrCreate() created = true, want false")
	}
	if got.Server.Port != want.Server.Port || got.Codex.Command != want.Codex.Command {
		t.Fatalf("LoadOrCreate() = %#v, want preserved values", got)
	}
	after, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("existing config was modified")
	}
}

func TestLoadRejectsInvalidJSONAndPaths(t *testing.T) {
	tests := map[string]string{
		"unknown field":              `{"unexpected":true}`,
		"duplicate key":              `{"server":{"port":7342,"port":8123}}`,
		"trailing value":             `{} {}`,
		"non-loopback":               `{"server":{"host":"0.0.0.0"}}`,
		"unix path traversal":        `{"storage":{"processed_directory":"../outside"}}`,
		"windows path traversal":     `{"storage":{"processed_directory":"..\\outside"}}`,
		"windows drive path":         `{"storage":{"processed_directory":"C:/outside"}}`,
		"windows network share path": `{"storage":{"processed_directory":"//server/share"}}`,
		"blank codex command":        `{"codex":{"command":" "}}`,
		"control in codex command":   `{"codex":{"command":"codex\u0007"}}`,
		"invalid reasoning effort":   `{"codex":{"reasoning_effort":"maximum"}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreate(root); err == nil {
				t.Fatal("LoadOrCreate() error = nil, want validation error")
			}
		})
	}
}

func writeConfig(t *testing.T, root string, cfg Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, FileName), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
