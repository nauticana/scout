package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// DecisionScope is the replay-stable position a decision is made at: a turn, a
// step of it, one loop iteration or tool call. A redelivered turn re-enters the
// same scopes in the same order, so a decision re-made under replay derives the
// key it had the first time.
type DecisionScope struct {
	path string

	mu   sync.Mutex
	seen map[string]int
}

type decisionScopeKey struct{}

// WithDecisionScope nests a scope under the context's current one. Parts must be
// identifiers that survive redelivery: a request id, a compiled step id, an
// idempotency key; never a clock or an attempt number.
func WithDecisionScope(ctx context.Context, parts ...any) context.Context {
	path := fmt.Sprint(parts...)
	if parent, ok := ctx.Value(decisionScopeKey{}).(*DecisionScope); ok {
		path = parent.path + "/" + path
	}
	return context.WithValue(ctx, decisionScopeKey{}, &DecisionScope{path: path, seen: make(map[string]int)})
}

// DecisionKeyFor returns the record's own key, else one derived from the context's
// scope, else "" for a decision made outside any scope, which stays append-only.
// The nth identical decision inside one scope gets the nth key.
func DecisionKeyFor(ctx context.Context, decision DecisionRecord) string {
	if decision.DecisionKey != "" {
		return decision.DecisionKey
	}
	scope, ok := ctx.Value(decisionScopeKey{}).(*DecisionScope)
	if !ok {
		return ""
	}
	identity := strings.Join([]string{decision.Category, decision.Action, decision.Resource}, "\x00")
	scope.mu.Lock()
	ordinal := scope.seen[identity]
	scope.seen[identity] = ordinal + 1
	scope.mu.Unlock()
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s\x00%d", decision.TenantID, scope.path, identity, ordinal))
	return hex.EncodeToString(sum[:])
}
