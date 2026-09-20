package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestTableTenantPolicyRepositoryNeverAssumesLimits(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}
	repository := &TableTenantPolicyRepository{DB: studioDBFake{qs: query}}
	if _, err := repository.GetRuntimePolicy(context.Background(), 7); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a tenant without a current policy must be ErrNotReady, got %v", err)
	}
	query.rows[qRuntimePolicyCurrent] = [][]any{{"standard", "shared", int64(12), int64(9000), int64(50), "USD", int64(30000)}}
	policy, err := repository.GetRuntimePolicy(context.Background(), 7)
	if err != nil || policy.MaxSteps != 12 || policy.MaxTokens != 9000 || policy.CostCurrency != "USD" || policy.TurnTimeout != 30*time.Second {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
}

func TestTableGuardrailConfigRepositoryIsImmutableAndReadsThePinnedVersion(t *testing.T) {
	rules := []byte(`{"rules":[]}`)
	sum := sha256.Sum256(rules)
	digest := hex.EncodeToString(sum[:])
	query := &studioQueryFake{rows: map[string][][]any{qGuardrailGet: {{"g1", string(rules), digest}}}, args: map[string][]any{}}
	repository := &TableGuardrailConfigRepository{DB: studioDBFake{qs: query}}
	ctx := context.Background()
	if err := repository.Publish(ctx, 7, "writer", domain.GuardrailConfig{Version: "g1", Rules: rules}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := repository.Publish(ctx, 7, "writer", domain.GuardrailConfig{Version: "g1", Rules: []byte(`{"rules":[1]}`)}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for different rules under one version, got %v", err)
	}
	if err := repository.Publish(ctx, 7, "writer", domain.GuardrailConfig{Version: "g1", Rules: rules, RulesDigest: "bad"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for a wrong digest, got %v", err)
	}
	if config, err := repository.Get(ctx, 7, "writer", "v1"); err != nil || config.Version != "" {
		t.Fatalf("an agent version pinning no guardrails gets the empty policy: %+v, %v", config, err)
	}
	query.rows[qGuardrailPinned] = [][]any{{"g1", string(rules), digest}}
	if config, err := repository.Get(ctx, 7, "writer", "v1"); err != nil || config.Version != "g1" || config.RulesDigest != digest {
		t.Fatalf("pinned config = %+v, %v", config, err)
	}
}

func standardPolicy() domain.RuntimePolicyVersion {
	return domain.RuntimePolicyVersion{Version: "p1", Policy: domain.TenantRuntimePolicy{
		PriorityClass: "standard", CapacityClass: "shared", MaxSteps: 12, MaxTokens: 9000,
		MaxCostMinorUnits: 50, CostCurrency: "USD", TurnTimeout: 30 * time.Second,
	}}
}

var standardPolicyRow = []any{"standard", "shared", int64(12), int64(9000), int64(50), "USD", int64(30000)}

func TestPublishRuntimePolicyIsImmutableAndMovesThePointerInOneTransaction(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{qRuntimePolicyGet: {standardPolicyRow}}, args: map[string][]any{}}
	repository := &TableTenantPolicyRepository{DB: studioDBFake{qs: query}}
	ctx := context.Background()
	if err := repository.PublishRuntimePolicy(ctx, 7, standardPolicy()); err != nil {
		t.Fatalf("PublishRuntimePolicy: %v", err)
	}
	if query.commits != 1 || query.args[qRuntimePolicyPoint][1] != "p1" || query.args[qRuntimePolicyInsert][8] != int64(30000) {
		t.Fatalf("commits = %d, queries = %v", query.commits, query.queries)
	}
	changed := standardPolicy()
	changed.Policy.MaxSteps = 99
	if err := repository.PublishRuntimePolicy(ctx, 7, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for different limits under one version, got %v", err)
	}
	if query.commits != 1 {
		t.Fatal("a conflicting version must not move the pointer")
	}
	invalid := standardPolicy()
	invalid.Policy.TurnTimeout = 0
	if err := repository.PublishRuntimePolicy(ctx, 7, invalid); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
