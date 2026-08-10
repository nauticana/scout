package mcptest

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/nauticana/scout/mcp"
)

// Manifest contains the code-owned identity and provider entries.
type Manifest struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Tools     []ManifestEntry `json:"tools"`
	Resources []ManifestEntry `json:"resources"`
}

// ManifestEntry is one published tool or resource.
type ManifestEntry struct {
	Name        string `json:"name"`
	URI         string `json:"uri"`
	Description string `json:"description"`
}

func (entry ManifestEntry) id() string {
	if entry.URI != "" {
		return entry.URI
	}
	return entry.Name
}

func LoadManifest(t *testing.T, path string) Manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return manifest
}

func AssertIdentityMatches(t *testing.T, manifest Manifest, name, version string) {
	t.Helper()
	if manifest.Name != name {
		t.Errorf("manifest name %q != code %q", manifest.Name, name)
	}
	if manifest.Version != version {
		t.Errorf("manifest version %q != code %q", manifest.Version, version)
	}
}

func AssertDescriptionsPresent(t *testing.T, manifest Manifest) {
	t.Helper()
	for _, entry := range append(append([]ManifestEntry{}, manifest.Tools...), manifest.Resources...) {
		if strings.TrimSpace(entry.Description) == "" {
			t.Errorf("manifest entry %q has an empty description", entry.id())
		}
	}
}

func AssertSameSet(t *testing.T, label string, code, manifest []string) {
	t.Helper()
	codeSet := make(map[string]bool, len(code))
	manifestSet := make(map[string]bool, len(manifest))
	for _, value := range code {
		codeSet[value] = true
	}
	for _, value := range manifest {
		manifestSet[value] = true
	}
	var missing, extra []string
	for _, value := range code {
		if !manifestSet[value] {
			missing = append(missing, value)
		}
	}
	for _, value := range manifest {
		if !codeSet[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s in code but missing from manifest: %v", label, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s in manifest but not registered in code: %v", label, extra)
	}
}

func AssertToolsMatchManifest(t *testing.T, tools []mcp.ToolProvider, manifestTools []string) {
	t.Helper()
	code := make([]string, len(tools))
	for i, provider := range tools {
		code[i] = provider.Name()
	}
	AssertSameSet(t, "tool", code, manifestTools)
}

func AssertResourcesMatchManifest(t *testing.T, resources []mcp.ResourceProvider, manifestURIs []string) {
	t.Helper()
	code := make([]string, len(resources))
	for i, provider := range resources {
		code[i] = provider.URI()
	}
	AssertSameSet(t, "resource", code, manifestURIs)
}

func AssertToolTextComplete(t *testing.T, tools []mcp.ToolProvider, bundle mcp.TextBundle) {
	t.Helper()
	for _, provider := range tools {
		if bundle.Tool(provider.Name()) == "" {
			t.Errorf("tool %q has no description in text bundle", provider.Name())
		}
	}
}
