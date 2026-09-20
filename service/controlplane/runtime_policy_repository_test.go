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
