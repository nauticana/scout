package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestVersionAliaserSwapIsCompareAndSet(t *testing.T) {
	query := &ingestQueryFake{}
	aliaser := &VersionAliaser{DB: ingestDBFake{query: query}}
	if err := aliaser.Swap(context.Background(), 7, "kb", "", "g1"); err != nil {
		t.Fatal(err)
	}
	insert := query.named(qAliasInsert)
	if len(insert) != 1 || insert[0].args[0] != int64(7) || insert[0].args[1] != "kb" || insert[0].args[2] != "g1" {
		t.Fatalf("insert args = %v", insert)
	}
	repoint := query.named(qAliasRepointManifest)
	if len(repoint) != 1 || repoint[0].args[0] != "g1" || repoint[0].args[1] != int64(7) || repoint[0].args[2] != "kb" || repoint[0].args[3] != "g1" || repoint[0].args[4] != "g1" {
		t.Fatalf("repoint args = %v", repoint)
	}
	if query.commits != 1 {
		t.Fatalf("commits = %d", query.commits)
	}

	query = &ingestQueryFake{rows: map[string][][]any{qAliasGet: {{"g1"}}, qAliasSwap: {{"g1"}}}}
	aliaser = &VersionAliaser{DB: ingestDBFake{query: query}}
	if err := aliaser.Swap(context.Background(), 7, "kb", "g0", "g2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale expected = %v", err)
	}
	if err := aliaser.Swap(context.Background(), 7, "kb", "", "g2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("exists but expected none = %v", err)
	}
	if err := aliaser.Swap(context.Background(), 7, "kb", "g1", "g2"); err != nil {
		t.Fatal(err)
	}
	swap := query.named(qAliasSwap)
	if len(swap) != 1 || swap[0].args[0] != "g2" || swap[0].args[1] != int64(7) || swap[0].args[2] != "kb" || swap[0].args[3] != "g1" {
		t.Fatalf("swap args = %v", swap)
	}
	if err := aliaser.Swap(context.Background(), 7, "kb", "g2", "g2"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("same version = %v", err)
	}
	active, err := aliaser.Active(context.Background(), 7, "kb")
	if err != nil || active != "g1" {
		t.Fatalf("active = %q, %v", active, err)
	}
	query.rows[qAliasGet] = nil
	if _, err := aliaser.Active(context.Background(), 7, "kb"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no alias = %v", err)
	}
	if query.rollbacks != 2 {
		t.Fatalf("rollbacks = %d", query.rollbacks)
	}
}
