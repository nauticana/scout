package charter

import (
	"context"
	"errors"
	"fmt"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ObjectStore maps stored documents to their enterprise_object ids and back, as charter's keel
// DocumentStore does.
type ObjectStore interface {
	ObjectIDs(ctx context.Context, partnerID int64, refs ...model.ObjectRef) ([]int64, error)
	ObjectRefs(ctx context.Context, partnerID int64, ids ...int64) ([]model.ObjectRef, error)
}

// DocumentObjects is the contract.CharterObjects over an ObjectStore. Namespace maps a tenant to
// the Charter namespace owning its documents.
type DocumentObjects struct {
	Store     ObjectStore
	Namespace func(tenantID int64) string
}

var (
	_ ObjectStore             = (*charterkeel.DocumentStore)(nil)
	_ contract.CharterObjects = (*DocumentObjects)(nil)
)

func (d *DocumentObjects) ObjectIDs(ctx context.Context, tenantID int64, kind string, identifiers ...string) ([]int64, error) {
	if d.Store == nil || d.Namespace == nil {
		return nil, fmt.Errorf("%w: charter objects need a store and a namespace", domain.ErrNotReady)
	}
	namespace := d.Namespace(tenantID)
	if namespace == "" {
		return nil, fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotReady, tenantID)
	}
	refs := make([]model.ObjectRef, len(identifiers))
	for i, identifier := range identifiers {
		refs[i] = model.ObjectRef{Kind: model.Kind(kind), Namespace: namespace, ID: identifier}
	}
	ids, err := d.Store.ObjectIDs(ctx, tenantID, refs...)
	return ids, objectError(err)
}

func (d *DocumentObjects) Identifiers(ctx context.Context, tenantID int64, ids ...int64) ([]string, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("%w: charter objects need a store", domain.ErrNotReady)
	}
	refs, err := d.Store.ObjectRefs(ctx, tenantID, ids...)
	if err != nil {
		return nil, objectError(err)
	}
	identifiers := make([]string, len(refs))
	for i, ref := range refs {
		identifiers[i] = ref.ID
	}
	return identifiers, nil
}

func objectError(err error) error {
	if errors.Is(err, corpus.ErrNotFound) {
		return fmt.Errorf("%w: %v", domain.ErrNotFound, err)
	}
	if err != nil {
		return fmt.Errorf("charter objects: %w", err)
	}
	return nil
}
