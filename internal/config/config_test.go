package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultMatchesExampleConfig(t *testing.T) {
	want, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(Default()) error = %v", err)
	}
	want = append(want, '\n')

	got, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.example.json) error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("config.example.json does not match config.Default()")
	}
}

func TestLoadUsesDefaultsWithoutCreatingConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-be-used")
	root := t.TempDir()

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load() = %#v, want compiled defaults", cfg)
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); !os.IsNotExist(err) {
		t.Fatalf("missing config was created: %v", err)
	}
}

func TestLoadAppliesPartialOverridesAndPreservesExistingConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, FileName)
	overrides := []byte(`{
  "server": {
    "port": 8123,
    "open_browser": false
  },
  "processing": {
    "workers": 4,
    "archive": {
      "max_entries": 500
    }
  },
  "codex": {
    "command": "custom-codex"
  }
}`)
	if err := os.WriteFile(path, overrides, 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}

	got, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Server.Port != 8123 || got.Server.OpenBrowser {
		t.Fatalf("server overrides = %#v", got.Server)
	}
	if got.Processing.Workers != 4 || got.Processing.Archive.MaxEntries != 500 {
		t.Fatalf("processing overrides = %#v", got.Processing)
	}
	if got.Codex.Command != "custom-codex" {
		t.Fatalf("codex command = %q, want custom-codex", got.Codex.Command)
	}
	defaults := Default()
	if got.Server.Host != defaults.Server.Host ||
		got.Processing.ThumbnailMaxDimension != defaults.Processing.ThumbnailMaxDimension ||
		got.Processing.Archive.MaxEntryBytes != defaults.Processing.Archive.MaxEntryBytes ||
		got.Codex.Model != defaults.Codex.Model {
		t.Fatalf("unspecified defaults were not preserved: %#v", got)
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
			if _, err := Load(root); err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
		})
	}
}
