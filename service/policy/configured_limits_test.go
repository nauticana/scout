package policy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestRestrictionLayerCacheSettings(t *testing.T) {
	if got := (&TableRestrictionLayers{}).ttl(); got != DefaultRestrictionTTL {
		t.Fatalf("zero TTL = %s, want %s", got, DefaultRestrictionTTL)
	}
	if got := (&TableRestrictionLayers{TTL: time.Minute}).ttl(); got != time.Minute {
		t.Fatalf("configured TTL = %s, want 1m", got)
	}
	ctx := context.Background()
	for name, entries := range map[string]int{"default": 0, "one entry": 1} {
		tables, store := newRestrictions(t)
		store.Entries = entries
		for range 2 {
			if _, err := store.Layers(ctx, 7); err != nil {
				t.Fatal(err)
			}
		}
		want := 2
		if entries == 1 {
			want = 4
		}
		if tables.reads != want {
			t.Fatalf("%s: store reads = %d, want %d", name, tables.reads, want)
		}
	}
	for name, set := range map[string]func(*TableRestrictionLayers){
		"negative TTL":     func(store *TableRestrictionLayers) { store.TTL = -time.Second },
		"negative entries": func(store *TableRestrictionLayers) { store.Entries = -1 },
	} {
		_, store := newRestrictions(t)
		set(store)
		if _, err := store.Layers(ctx, 7); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want a validation refusal", name, err)
		}
	}
}

func TestStudioRestrictionEntriesDefaultAndRefusal(t *testing.T) {
	ctx := context.Background()
	fixture := newStudioRestrictions(t, domain.RestrictionReplaceTenant)
	denials := make([]domain.PolicyStatement, DefaultStudioRestrictionEntries+1)
	for index := range denials {
		denials[index] = denial(fmt.Sprintf("d%d", index))
	}
	if _, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, domain.RestrictionLayer{Denials: denials}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a layer above the default must be refused, got %v", err)
	}
	if _, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, domain.RestrictionLayer{Denials: denials[:DefaultStudioRestrictionEntries]}, ""); err != nil {
		t.Fatalf("a layer at the default: %v", err)
	}
	fixture.service.MaxEntries = -1
	if _, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, domain.RestrictionLayer{}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("a negative entry limit must be refused")
	}
}
