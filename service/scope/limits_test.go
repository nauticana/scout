package scope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestLimitsCanonicalValueSpellsEveryFieldOnce(t *testing.T) {
	first, err := CanonicalValue(domain.ResourceLimits, []byte(`{"max_steps":5,"turns_per_minute":30}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalValue(domain.ResourceLimits, []byte(` {"turns_per_minute":30, "max_steps":5} `))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"turns_per_minute":30,"max_steps":5,"max_tool_calls":0,"max_duration_seconds":0}`
	if string(first) != want || string(second) != want {
		t.Fatalf("canonical = %s and %s, want %s", first, second, want)
	}
}

func TestLimitsCanonicalValueRefusesWhatIsNotANonNegativeInteger(t *testing.T) {
	for name, raw := range map[string]string{
		"negative":      `{"max_steps":-1}`,
		"fraction":      `{"max_tool_calls":1.5}`,
		"exponent":      `{"max_tool_calls":1e2}`,
		"string":        `{"turns_per_minute":"10"}`,
		"unknown field": `{"max_tokens":10}`,
		"too large":     fmt.Sprintf(`{"max_duration_seconds":%d}`, int64(domain.MaxExecutionLimit)+1),
		"trailing":      `{"max_steps":1}{}`,
		"not an object": `[1,2]`,
	} {
		if _, err := CanonicalValue(domain.ResourceLimits, []byte(raw)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	if _, err := CanonicalValue(domain.ResourceLimits, []byte(fmt.Sprintf(`{"max_duration_seconds":%d}`, domain.MaxExecutionLimit))); err != nil {
		t.Fatalf("the largest limit must be accepted: %v", err)
	}
}

func TestCompileKeepsTheTighterOfEachLimit(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceLimits, `{"turns_per_minute":10,"max_steps":8,"max_tool_calls":0,"max_duration_seconds":120}`),
		binding("unit", domain.ResourceLimits, `{"turns_per_minute":10,"max_steps":6,"max_tool_calls":0,"max_duration_seconds":120}`),
		binding("agent", domain.ResourceLimits, `{"turns_per_minute":20,"max_steps":0,"max_tool_calls":3,"max_duration_seconds":60}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var limits limitsValue
	if err := json.Unmarshal(resource(t, release, domain.ResourceLimits).Value, &limits); err != nil {
		t.Fatal(err)
	}
	// The agent's raise and its unset keep the inherited limit; it may set what the parent leaves open.
	if limits != (limitsValue{TurnsPerMinute: 10, MaxSteps: 6, MaxToolCalls: 3, MaxDurationSeconds: 60}) {
		t.Fatalf("limits = %+v", limits)
	}
}

func TestLimitsNarrowAtMostPerField(t *testing.T) {
	checker := &LatticeChecker{}
	parent := []byte(`{"turns_per_minute":10,"max_steps":8,"max_tool_calls":0,"max_duration_seconds":120}`)
	for name, child := range map[string]string{
		"lowered":            `{"turns_per_minute":5,"max_steps":8,"max_tool_calls":0,"max_duration_seconds":30}`,
		"set where open":     `{"turns_per_minute":10,"max_steps":8,"max_tool_calls":99,"max_duration_seconds":120}`,
		"unchanged":          string(parent),
		"lowest permissible": `{"turns_per_minute":1,"max_steps":1,"max_tool_calls":1,"max_duration_seconds":1}`,
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceLimits, parent, []byte(child)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, child := range map[string]string{
		"turns_per_minute":     `{"turns_per_minute":11,"max_steps":8,"max_tool_calls":0,"max_duration_seconds":120}`,
		"max_steps":            `{"turns_per_minute":10,"max_steps":0,"max_tool_calls":0,"max_duration_seconds":120}`,
		"max_duration_seconds": `{"turns_per_minute":10,"max_steps":8,"max_tool_calls":0,"max_duration_seconds":121}`,
	} {
		err := checker.CheckNarrowing(context.Background(), domain.ResourceLimits, parent, []byte(child))
		if !errors.Is(err, domain.ErrAuthorityExceeded) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error = %v, want the raised or unset field named", name, err)
		}
	}
	if err := checker.CheckNarrowing(context.Background(), domain.ResourceLimits, nil, []byte(`{"max_steps":99}`)); err != nil {
		t.Fatalf("nothing inherited narrows nothing: %v", err)
	}
}

func TestLimitsMergeByReplaceOnly(t *testing.T) {
	_, err := LimitsMerger{}.Merge(context.Background(), []byte(`{"max_steps":4}`), domain.ScopedBinding{
		ResourceKind: domain.ResourceLimits, MergeMode: domain.MergeAppend, Value: []byte(`{"max_steps":2}`),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	registry, err := NewMergerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if merger, err := registry.MergerFor(context.Background(), domain.ResourceLimits); err != nil || merger.Kind() != domain.ResourceLimits {
		t.Fatalf("merger = %v, error = %v", merger, err)
	}
}

// The seeded resource_kind catalog is what Studio and products read; it must name each
// platform kind with the rule this package applies to it.
func TestPlatformNarrowingRulesMatchTheSeededCatalog(t *testing.T) {
	seed, err := os.ReadFile("../../schema/seed/catalog/catalog.yml")
	if err != nil {
		t.Fatal(err)
	}
	for kind, rule := range PlatformNarrowingRules {
		if row := fmt.Sprintf("- [%s, %s, ", kind, rule); !strings.Contains(string(seed), row) {
			t.Errorf("config_resource_kind has no row %q", row)
		}
	}
}
