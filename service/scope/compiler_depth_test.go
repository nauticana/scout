package scope

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestNewCompilerDepthIsBoundedByTheChainCeiling(t *testing.T) {
	registry, err := NewMergerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for depth, want := range map[int]int{0: DefaultMaxScopeDepth, 3: 3, MaxScopeChainDepth: MaxScopeChainDepth} {
		compiler, err := NewCompiler(stubRepository{}, registry, &LatticeChecker{}, depth)
		if err != nil || compiler.MaxDepth != want {
			t.Fatalf("depth %d: compiler = %+v, %v; want %d", depth, compiler, err, want)
		}
	}
	for _, depth := range []int{-1, MaxScopeChainDepth + 1} {
		if _, err := NewCompiler(stubRepository{}, registry, &LatticeChecker{}, depth); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("depth %d: error = %v, want a validation refusal", depth, err)
		}
	}
}
