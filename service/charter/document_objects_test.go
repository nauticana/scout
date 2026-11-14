package charter

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

// objectStore numbers the documents it stores from 101, like enterprise_object ids.
type objectStore struct {
	refs []model.ObjectRef
	asks int
}

func (s *objectStore) ObjectIDs(_ context.Context, _ int64, refs ...model.ObjectRef) ([]int64, error) {
	s.asks++
	ids := make([]int64, len(refs))
	for i, ref := range refs {
		at := slices.Index(s.refs, ref)
		if at < 0 {
			return nil, fmt.Errorf("%w: %s %s:%s", corpus.ErrNotFound, ref.Kind, ref.Namespace, ref.ID)
		}
		ids[i] = int64(101 + at)
	}
	return ids, nil
}

func (s *objectStore) ObjectRefs(_ context.Context, _ int64, ids ...int64) ([]model.ObjectRef, error) {
	refs := make([]model.ObjectRef, len(ids))
	for i, id := range ids {
		if id < 101 || int(id-101) >= len(s.refs) {
			return nil, fmt.Errorf("%w: object %d", corpus.ErrNotFound, id)
		}
		refs[i] = s.refs[id-101]
	}
	return refs, nil
}

func TestDocumentObjectsResolveInTheTenantNamespace(t *testing.T) {
	ctx := context.Background()
	store := &objectStore{refs: []model.ObjectRef{
		{Kind: model.KindAssignment, Namespace: "tenant.example", ID: "ASGN-1"},
		{Kind: model.KindSodException, Namespace: "tenant.example", ID: "EXC-1"},
	}}
	objects := &DocumentObjects{Store: store, Namespace: func(int64) string { return "tenant.example" }}
	ids, err := objects.ObjectIDs(ctx, 7, domain.CharterSodExceptionKind, "EXC-1")
	if err != nil || !slices.Equal(ids, []int64{102}) {
		t.Fatalf("ObjectIDs = %v, %v", ids, err)
	}
	if _, err = objects.ObjectIDs(ctx, 7, domain.CharterAssignmentKind, "EXC-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another kind: want ErrNotFound, got %v", err)
	}
	identifiers, err := objects.Identifiers(ctx, 7, 102, 101)
	if err != nil || !slices.Equal(identifiers, []string{"EXC-1", "ASGN-1"}) {
		t.Fatalf("Identifiers = %v, %v", identifiers, err)
	}
	if _, err = objects.Identifiers(ctx, 7, 9); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown id: want ErrNotFound, got %v", err)
	}
	objects.Namespace = func(int64) string { return "" }
	if _, err = objects.ObjectIDs(ctx, 7, domain.CharterAssignmentKind, "ASGN-1"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no namespace: want ErrNotReady, got %v", err)
	}
	if _, err = (&DocumentObjects{}).Identifiers(ctx, 7, 101); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("zero value: want ErrNotReady, got %v", err)
	}
}

// Scout's kind constants name Charter's kinds.
func TestCharterKindsAgree(t *testing.T) {
	if domain.CharterAssignmentKind != string(model.KindAssignment) || domain.CharterSodExceptionKind != string(model.KindSodException) {
		t.Fatalf("kinds %q %q", domain.CharterAssignmentKind, domain.CharterSodExceptionKind)
	}
}
