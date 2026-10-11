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

// StudioFeatureUsage is the Studio feature StudioUsageHandler serves.
const StudioFeatureUsage = "usage"

// StudioUsageHandler serves an agent's usage and cost report.
type StudioUsageHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioUsageBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioUsageHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureUsage, Version: 1, Actions: []string{string(domain.StudioView)}}
}

// Routes returns the usage route: ?agent_name=&from=&to= with RFC 3339 times.
func (h *StudioUsageHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioUsagePath: query(&h.AbstractHandler, h.usage)}
}

func (h *StudioUsageHandler) usage(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	from, fromErr := time.Parse(time.RFC3339, query.Get("from"))
	to, toErr := time.Parse(time.RFC3339, query.Get("to"))
	if fromErr != nil || toErr != nil {
		return nil, badRequest("from and to must be RFC 3339 times")
	}
	lines, err := h.Backend.Usage(ctx, actor, query.Get("agent_name"), from, to)
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentUsageLine, 0, len(lines))
	for _, line := range lines {
		items = append(items, api.AgentUsageLine{
			Version: line.Release, ActorKind: string(line.OnBehalfOf.Kind), ActorID: line.OnBehalfOf.ID, WorkRef: line.WorkRef,
			Currency: line.Currency, Exponent: line.Exponent, Turns: line.Turns, InputTokens: line.InputTokens,
			OutputTokens: line.OutputTokens, ToolCalls: line.ToolCalls, CostMinorUnits: line.CostMinorUnits,
		})
	}
	return items, nil
}
