package scope

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SetMerger combines JSON string arrays. Append widens and is therefore rejected
// by the narrowing checker on every kind whose rule is subset; it stays available
// because a product may register the merger against a kind that is not authority.
type SetMerger struct{ ResourceKind domain.ResourceKind }

func (m SetMerger) Kind() domain.ResourceKind { return m.ResourceKind }

func (m SetMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	parent, err := decodeSet(inherited)
	if err != nil {
		return nil, err
	}
	child, err := decodeSet(override.Value)
	if err != nil {
		return nil, err
	}
	switch override.MergeMode {
	case domain.MergeReplace, "":
		return encodeSet(child)
	case domain.MergeAppend:
		return encodeSet(append(append([]string(nil), parent...), child...))
	case domain.MergeIntersect:
		keep := make(map[string]struct{}, len(parent))
		for _, item := range parent {
			keep[item] = struct{}{}
		}
		common := make([]string, 0, len(child))
		for _, item := range child {
			if _, ok := keep[item]; ok {
				common = append(common, item)
			}
		}
		return encodeSet(common)
	default:
		return nil, fmt.Errorf("%w: unsupported merge mode %q", domain.ErrValidation, override.MergeMode)
	}
}

// PromptMerger combines instruction and output text. Append keeps the inherited
// text and adds the child's below it; replace substitutes it outright.
type PromptMerger struct{}

func (PromptMerger) Kind() domain.ResourceKind { return domain.ResourcePromptSection }

func (PromptMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	parent, err := decodePrompt(inherited)
	if err != nil {
		return nil, err
	}
	child, err := decodePrompt(override.Value)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(child.Instruction) == "" {
		return nil, fmt.Errorf("%w: prompt instruction is required", domain.ErrValidation)
	}
	merged := child
	switch override.MergeMode {
	case domain.MergeReplace, "":
	case domain.MergeAppend:
		merged.Instruction = joinText(parent.Instruction, child.Instruction)
		merged.Output = joinText(parent.Output, child.Output)
	default:
		return nil, fmt.Errorf("%w: prompt sections support replace and append only", domain.ErrValidation)
	}
	return json.Marshal(merged)
}

// PolicyMerger combines statement lists. Order does not matter to evaluation —
// deny wins regardless — so a merge is a concatenation the narrowing rule then
// checks: a child may drop an allow or add a deny, never add an allow.
type PolicyMerger struct{}

func (PolicyMerger) Kind() domain.ResourceKind { return domain.ResourcePolicy }

func (PolicyMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	parent, err := decodeStatements(inherited)
	if err != nil {
		return nil, err
	}
	child, err := decodeStatements(override.Value)
	if err != nil {
		return nil, err
	}
	switch override.MergeMode {
	case domain.MergeReplace, "":
		return encodeStatements(child)
	case domain.MergeAppend:
		return encodeStatements(append(append([]domain.PolicyStatement(nil), parent...), child...))
	default:
		return nil, fmt.Errorf("%w: policies support replace and append only", domain.ErrValidation)
	}
}

// BudgetMerger takes the tighter of each inherited and child limit, so a child
// cannot raise a ceiling even with a replace binding.
type BudgetMerger struct{}

func (BudgetMerger) Kind() domain.ResourceKind { return domain.ResourceBudget }

func (BudgetMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if override.MergeMode != domain.MergeReplace && override.MergeMode != "" {
		return nil, fmt.Errorf("%w: budgets support replace only", domain.ErrValidation)
	}
	parent, err := decodeBudget(inherited)
	if err != nil {
		return nil, err
	}
	child, err := decodeBudget(override.Value)
	if err != nil {
		return nil, err
	}
	if parent.Currency != "" && child.Currency != "" && parent.Currency != child.Currency {
		return nil, fmt.Errorf("%w: budget currency %q cannot override %q", domain.ErrValidation, child.Currency, parent.Currency)
	}
	merged := budgetValue{Tokens: child.Tokens, CostMinorUnits: child.CostMinorUnits, Currency: firstNonEmpty(child.Currency, parent.Currency)}
	if parent.Tokens > 0 {
		merged.Tokens = minPositive(parent.Tokens, child.Tokens)
	}
	if parent.CostMinorUnits > 0 {
		merged.CostMinorUnits = minPositive(parent.CostMinorUnits, child.CostMinorUnits)
	}
	return json.Marshal(merged)
}

// AutonomyMerger keeps the lower of the inherited and child operating modes.
type AutonomyMerger struct{}

func (AutonomyMerger) Kind() domain.ResourceKind { return domain.ResourceAutonomy }

func (AutonomyMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if override.MergeMode != domain.MergeReplace && override.MergeMode != "" {
		return nil, fmt.Errorf("%w: autonomy supports replace only", domain.ErrValidation)
	}
	child, err := decodeAutonomy(override.Value)
	if err != nil {
		return nil, err
	}
	if len(inherited) == 0 {
		return json.Marshal(child)
	}
	parent, err := decodeAutonomy(inherited)
	if err != nil {
		return nil, err
	}
	if autonomyRank[parent.Mode] < autonomyRank[child.Mode] {
		return json.Marshal(parent)
	}
	if autonomyRank[child.Mode] < autonomyRank[parent.Mode] || child.Mode != domain.AutonomyBounded {
		return json.Marshal(child)
	}
	var overlaps bool
	child.WindowFrom, child.WindowTo, overlaps = intersectWindow(parent.WindowFrom, parent.WindowTo, child.WindowFrom, child.WindowTo)
	if !overlaps {
		child.Mode = domain.AutonomyExecuteWithApproval
	}
	return json.Marshal(child)
}

func intersectWindow(parentFrom, parentTo, childFrom, childTo int) (int, int, bool) {
	if parentTo == 0 {
		return childFrom, childTo, true
	}
	if childTo == 0 {
		return parentFrom, parentTo, true
	}
	from, to := max(parentFrom, childFrom), min(parentTo, childTo)
	if from >= to {
		return 0, 0, false
	}
	return from, to, true
}

// MergerRegistry resolves mergers by resource kind.
type MergerRegistry struct {
	mergers map[domain.ResourceKind]contract.ResourceMerger
}

// NewMergerRegistry registers the platform mergers and then the supplied ones,
// so a product may replace a platform kind but never leave one unregistered.
func NewMergerRegistry(extra ...contract.ResourceMerger) (*MergerRegistry, error) {
	registry := &MergerRegistry{mergers: map[domain.ResourceKind]contract.ResourceMerger{}}
	platform := []contract.ResourceMerger{
		PromptMerger{}, BudgetMerger{}, AutonomyMerger{}, PolicyMerger{},
		SetMerger{ResourceKind: domain.ResourceTool},
		SetMerger{ResourceKind: domain.ResourceKnowledge},
		SetMerger{ResourceKind: domain.ResourceModel},
		SetMerger{ResourceKind: domain.ResourceEntitlement},
	}
	for _, merger := range append(platform, extra...) {
		if merger == nil || strings.TrimSpace(string(merger.Kind())) == "" {
			return nil, fmt.Errorf("merger registry: every merger must name a resource kind")
		}
		registry.mergers[merger.Kind()] = merger
	}
	return registry, nil
}

// MergerFor returns the merger registered for a resource kind.
func (r *MergerRegistry) MergerFor(_ context.Context, kind domain.ResourceKind) (contract.ResourceMerger, error) {
	merger, ok := r.mergers[kind]
	if !ok {
		return nil, fmt.Errorf("%w: no merger registered for resource kind %q", domain.ErrValidation, kind)
	}
	return merger, nil
}

// Kinds returns the registered resource kinds in a stable order.
func (r *MergerRegistry) Kinds() []domain.ResourceKind {
	kinds := make([]domain.ResourceKind, 0, len(r.mergers))
	for kind := range r.mergers {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

func joinText(parent, child string) string {
	switch {
	case strings.TrimSpace(parent) == "":
		return child
	case strings.TrimSpace(child) == "":
		return parent
	default:
		return parent + "\n" + child
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func minPositive(parent, child int64) int64 {
	if child <= 0 || child > parent {
		return parent
	}
	return child
}

var (
	_ contract.ResourceMerger         = SetMerger{}
	_ contract.ResourceMerger         = PolicyMerger{}
	_ contract.ResourceMerger         = PromptMerger{}
	_ contract.ResourceMerger         = BudgetMerger{}
	_ contract.ResourceMerger         = AutonomyMerger{}
	_ contract.ResourceMergerRegistry = (*MergerRegistry)(nil)
)
