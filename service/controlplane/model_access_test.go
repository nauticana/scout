package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestModelAccessGrantsAndRevokesOneModel(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}
	access := &ModelAccess{DB: studioDBFake{qs: query}}
	ctx := context.Background()
	reference := domain.ModelReference{ProviderID: " openai ", ModelID: " text-embedding-3-small "}
	if err := access.Grant(ctx, 7, reference, ""); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	granted := query.args[qModelAccessGrant]
	if len(granted) != 4 || granted[1] != "openai" || granted[2] != "text-embedding-3-small" || granted[3] != DefaultPriorityClass {
		t.Fatalf("grant args = %v", granted)
	}
	if err := access.Grant(ctx, 7, reference, "batch"); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	if query.args[qModelAccessGrant][3] != "batch" {
		t.Fatalf("a repeated grant updates the priority class, args = %v", query.args[qModelAccessGrant])
	}
	if err := access.Revoke(ctx, 7, reference); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked := query.args[qModelAccessRevoke]; len(revoked) != 3 || revoked[0] != int64(7) {
		t.Fatalf("revoke args = %v", revoked)
	}
}

func TestModelAccessRequiresATenantAndAModel(t *testing.T) {
	access := &ModelAccess{DB: studioDBFake{qs: &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}}}
	ctx := context.Background()
	if err := access.Grant(ctx, 0, domain.ModelReference{ProviderID: "openai", ModelID: "m"}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("grant without a tenant = %v", err)
	}
	if err := access.Revoke(ctx, 7, domain.ModelReference{ProviderID: "openai"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("revoke without a model = %v", err)
	}
}
