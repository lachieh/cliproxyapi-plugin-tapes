// Command registry records a plugin release in registry.json.
//
// The CLIProxyAPI store's github-release installs always take a repository's
// latest release, so a repository with several plugins lists each release as
// a direct install instead (registry schema 2): download URLs and checksums
// for every platform. The entry's metadata comes from the plugin's store.json;
// this sets its version and artifacts from the release's zips and moves the
// version it replaces into versions.
//
//	go run ./scripts/registry -plugin plugins/tapes -version 0.4.0 -tag tapes/v0.4.0 -dist dist
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// keepVersions bounds how many earlier versions stay installable.
const keepVersions = 10

type artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type install struct {
	Type      string     `json:"type"`
	Artifacts []artifact `json:"artifacts"`
}

type version struct {
	Version string  `json:"version"`
	Install install `json:"install"`
}

func main() {
	path := flag.String("registry", "registry.json", "registry file to update")
	dir := flag.String("plugin", "", "plugin directory holding store.json")
	ver := flag.String("version", "", "released version, without a leading v")
	tag := flag.String("tag", "", "release tag")
	dist := flag.String("dist", "dist", "directory holding the release zips")
	repo := flag.String("repo", "https://github.com/lachieh/cliproxyapi-plugins", "repository URL")
	flag.Parse()
	if *dir == "" || *ver == "" || *tag == "" {
		flag.Usage()
		os.Exit(2)
	}
	meta, id, err := readMeta(filepath.Join(*dir, "store.json"))
	if err != nil {
		log.Fatal(err)
	}
	artifacts, err := collect(*dist, id, *ver, *repo, *tag)
	if err != nil {
		log.Fatal(err)
	}
	if err := update(*path, meta, id, version{Version: *ver, Install: install{Type: "direct", Artifacts: artifacts}}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("registry: %s %s with %d artifacts\n", id, *ver, len(artifacts))
}

// readMeta reads a plugin's store metadata: everything in its registry entry
// except version, install and versions.
func readMeta(path string) (map[string]json.RawMessage, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	var id string
	_ = json.Unmarshal(meta["id"], &id)
	if id == "" {
		return nil, "", fmt.Errorf("%s: missing id", path)
	}
	for _, k := range []string{"version", "install", "versions"} {
		if _, ok := meta[k]; ok {
			return nil, "", fmt.Errorf("%s: %s is set by releases, not store.json", path, k)
		}
	}
	return meta, id, nil
}

// collect describes each <id>_<version>_<goos>_<goarch>.zip in dist.
func collect(dist, id, ver, repo, tag string) ([]artifact, error) {
	prefix := id + "_" + ver + "_"
	zips, err := filepath.Glob(filepath.Join(dist, prefix+"*.zip"))
	if err != nil {
		return nil, err
	}
	if len(zips) == 0 {
		return nil, fmt.Errorf("no %s*.zip in %s", prefix, dist)
	}
	sort.Strings(zips)
	var out []artifact
	for _, zip := range zips {
		name := filepath.Base(zip)
		platform := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".zip"), "_", 2)
		if len(platform) != 2 {
			return nil, fmt.Errorf("%s: want %s<goos>_<goarch>.zip", name, prefix)
		}
		data, err := os.ReadFile(zip)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out = append(out, artifact{
			GOOS:   platform[0],
			GOARCH: platform[1],
			URL:    repo + "/releases/download/" + url.PathEscape(tag) + "/" + name,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		})
	}
	return out, nil
}

// update rewrites one plugin entry, adding it if this is its first release,
// and leaves the other entries untouched.
func update(path string, meta map[string]json.RawMessage, id string, release version) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var registry struct {
		SchemaVersion int                          `json:"schema_version"`
		Plugins       []map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if registry.SchemaVersion != 2 {
		return fmt.Errorf("%s: direct installs need schema_version 2, got %d", path, registry.SchemaVersion)
	}
	var entry map[string]json.RawMessage
	for _, p := range registry.Plugins {
		var pid string
		_ = json.Unmarshal(p["id"], &pid)
		if pid == id {
			entry = p
		}
	}
	if entry == nil {
		entry = map[string]json.RawMessage{}
		registry.Plugins = append(registry.Plugins, entry)
	}
	for k := range entry {
		if k != "version" && k != "install" && k != "versions" {
			delete(entry, k)
		}
	}
	for k, v := range meta {
		entry[k] = v
	}

	var versions []version
	if v, ok := entry["versions"]; ok {
		if err := json.Unmarshal(v, &versions); err != nil {
			return fmt.Errorf("%s versions: %w", id, err)
		}
	}
	var current version
	_ = json.Unmarshal(entry["version"], &current.Version)
	if inst, ok := entry["install"]; ok {
		_ = json.Unmarshal(inst, &current.Install)
	}
	// Re-running for the same version replaces it rather than listing it twice.
	kept := []version{}
	if current.Version != "" && current.Version != release.Version && len(current.Install.Artifacts) > 0 {
		kept = append(kept, current)
	}
	for _, v := range versions {
		if v.Version != release.Version && v.Version != current.Version {
			kept = append(kept, v)
		}
	}
	if len(kept) > keepVersions {
		kept = kept[:keepVersions]
	}

	entry["version"] = mustJSON(release.Version)
	entry["install"] = mustJSON(release.Install)
	if len(kept) > 0 {
		entry["versions"] = mustJSON(kept)
	} else {
		delete(entry, "versions")
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(orderedRegistry(registry.SchemaVersion, registry.Plugins)); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// fieldOrder keeps registry entries readable: metadata first, artifacts last.
var fieldOrder = []string{"id", "name", "description", "author", "repository", "logo", "homepage", "license", "tags", "auth_required", "version", "install", "versions"}

func orderedRegistry(schema int, plugins []map[string]json.RawMessage) json.RawMessage {
	var b strings.Builder
	fmt.Fprintf(&b, `{"schema_version":%d,"plugins":[`, schema)
	for i, p := range plugins {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		seen := map[string]bool{}
		first := true
		write := func(k string) {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.Write(mustJSON(k))
			b.WriteByte(':')
			b.Write(p[k])
			seen[k] = true
		}
		for _, k := range fieldOrder {
			if _, ok := p[k]; ok {
				write(k)
			}
		}
		var rest []string
		for k := range p {
			if !seen[k] {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			write(k)
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return json.RawMessage(b.String())
}

func mustJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
