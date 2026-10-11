package handler

import (
	"context"
	"net/http"
	"net/url"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureLimits is the Studio feature StudioLimitsHandler serves.
const StudioFeatureLimits = "limits"

// StudioLimitsHandler serves the execution limits and budget frozen into an agent's stable
// release, with the agent's current use of them.
type StudioLimitsHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioLimitsBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioLimitsHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureLimits, Version: 1, Actions: []string{string(domain.StudioView)}}
}

// Routes returns the limits route: ?agent_name=.
func (h *StudioLimitsHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioLimitsPath: query(&h.AbstractHandler, h.limits)}
}

func (h *StudioLimitsHandler) limits(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	report, err := h.Backend.Limits(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	result := api.AgentLimits{
		AgentName: report.AgentID, Version: report.Release, TurnsLastMinute: report.TurnsLastMinute,
		Limits: api.AgentExecutionLimits{
			TurnsPerMinute: report.Limits.TurnsPerMinute, MaxSteps: report.Limits.MaxSteps, MaxToolCalls: report.Limits.MaxToolCalls,
			MaxDurationSeconds: int64(report.Limits.MaxDuration / time.Second),
		},
	}
	if budget := report.Budget; budget != nil {
		result.Budget = &api.AgentBudget{
			Tokens: budget.WindowTokens, CostMinorUnits: budget.WindowCostMinorUnits, Currency: budget.Currency,
			WindowSeconds: int64(budget.Window / time.Second),
		}
	}
	if spend := report.Spend; spend != nil {
		result.Spend = &api.AgentBudgetSpend{
			PeriodStart: spend.PeriodStart, PeriodEnd: spend.PeriodEnd, Tokens: spend.Tokens,
			CostMinorUnits: spend.CostMinorUnits, Currency: spend.Currency, Exponent: spend.Exponent,
		}
	}
	return result, nil
}
