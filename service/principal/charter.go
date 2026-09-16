package principal

import (
	"fmt"
	"time"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/domain"
)

// Scout's agent principal kind is Charter's: both register the same keel kind
// against agent_permission, so the literal must be one symbol.
var _ = map[bool]struct{}{false: {}, string(charterkeel.AgentPrincipalKind) == string(domain.PrincipalAgent): {}}

// BudgetLimitKind names the per-hop spend bound in a Charter delegation chain.
const BudgetLimitKind = "spend"

// CharterChain maps a Scout authority chain onto Charter's carrier so the
// invoker's DelegationPolicy can verify the presented path. A hop with no end
// date becomes an open-ended validity, never a nil one — Charter fails closed
// on nil. Scout's service principals have no Charter identity kind and fail.
func CharterChain(chain domain.AuthorityChain, namespace string) (model.AuthorityChain, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	out := make(model.AuthorityChain, 0, len(chain))
	for index, hop := range chain {
		delegator, err := charterActor(hop.Grantor, namespace)
		if err != nil {
			return nil, fmt.Errorf("authority hop %d: %w", index, err)
		}
		converted := model.AuthorityHop{
			GrantRef:         model.Ref{Namespace: namespace, ID: hop.GrantID},
			Delegator:        delegator,
			RemainingDepth:   hop.MaxDepth,
			Validity:         &model.Validity{From: charterDate(hop.NotBefore), To: charterDate(hop.NotAfter)},
			ApprovalRequired: hop.ApprovalRequired,
		}
		if hop.BudgetMinorUnits > 0 {
			exponent, ok := common.CurrencyExponent(hop.Currency)
			if !ok {
				return nil, fmt.Errorf("%w: authority hop %d has unknown currency %q", domain.ErrValidation, index, hop.Currency)
			}
			converted.Limits = []model.Limit{{
				LimitKind: BudgetLimitKind, Operator: "lte", Value: hop.BudgetMinorUnits,
				Currency: hop.Currency, CurrencyExponent: &exponent,
			}}
		}
		out = append(out, converted)
	}
	return out, nil
}

func charterActor(ref domain.PrincipalRef, namespace string) (model.ObjectRef, error) {
	switch ref.Kind {
	case domain.PrincipalAgent:
		return model.ObjectRef{Kind: model.KindAgentIdentity, ID: ref.ID, Namespace: namespace}, nil
	case domain.PrincipalHuman:
		return model.ObjectRef{Kind: model.KindHumanIdentity, ID: ref.ID, Namespace: namespace}, nil
	}
	return model.ObjectRef{}, fmt.Errorf("%w: principal kind %q has no Charter identity", domain.ErrValidation, ref.Kind)
}

// charterDate renders a calendar date; a zero time is Charter's open end.
func charterDate(t time.Time) model.Date {
	if t.IsZero() {
		return ""
	}
	return model.Date(t.UTC().Format("2006-01-02"))
}
