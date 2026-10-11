package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"plugins":[{"id":"other","name":"Other","version":"1.0.0","install":{"type":"direct","artifacts":[]}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]json.RawMessage{"id": json.RawMessage(`"tapes"`), "name": json.RawMessage(`"tapes"`)}
	release := func(v string) version {
		return version{Version: v, Install: install{Type: "direct", Artifacts: []artifact{{GOOS: "linux", GOARCH: "amd64", URL: "https://x/" + v, SHA256: "ab", Size: 1}}}}
	}
	read := func() (string, []string) {
		raw, _ := os.ReadFile(path)
		var reg struct {
			Plugins []struct {
				ID       string    `json:"id"`
				Version  string    `json:"version"`
				Versions []version `json:"versions"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(raw, &reg); err != nil {
			t.Fatal(err)
		}
		if len(reg.Plugins) != 2 || reg.Plugins[0].ID != "other" {
			t.Fatalf("plugins = %+v", reg.Plugins)
		}
		var older []string
		for _, v := range reg.Plugins[1].Versions {
			older = append(older, v.Version)
		}
		return reg.Plugins[1].Version, older
	}

	steps := []struct {
		release string
		current string
		older   []string
	}{
		{"0.4.0", "0.4.0", nil},               // first release adds the entry
		{"0.5.0", "0.5.0", []string{"0.4.0"}}, // the replaced version moves to versions
		{"0.5.0", "0.5.0", []string{"0.4.0"}}, // re-running a release changes nothing
		{"0.6.0", "0.6.0", []string{"0.5.0", "0.4.0"}},
	}
	for _, step := range steps {
		if err := update(path, meta, "tapes", release(step.release)); err != nil {
			t.Fatal(err)
		}
		current, older := read()
		if current != step.current || len(older) != len(step.older) {
			t.Fatalf("after %s: version %s, versions %v; want %s, %v", step.release, current, older, step.current, step.older)
		}
		for i := range older {
			if older[i] != step.older[i] {
				t.Fatalf("after %s: versions %v, want %v", step.release, older, step.older)
			}
		}
	}

	for i := 0; i < keepVersions+3; i++ {
		if err := update(path, meta, "tapes", release("1.0."+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	if _, older := read(); len(older) != keepVersions {
		t.Errorf("kept %d versions, want %d", len(older), keepVersions)
	}
}
