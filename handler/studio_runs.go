package handler

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureRuns is the Studio feature StudioRunsHandler serves.
const StudioFeatureRuns = "runs"

// StudioRunsHandler serves an agent's run history and the trace of one run.
type StudioRunsHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioRunsBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioRunsHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureRuns, Version: 2, Actions: []string{string(domain.StudioView)}}
}

// Routes returns the runs route, ?agent_name=&status=&before=&limit=, and the trace route, ?agent_name=&run_id=.
func (h *StudioRunsHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioRunsPath:     query(&h.AbstractHandler, h.runs),
		api.StudioRunTracePath: query(&h.AbstractHandler, h.trace),
	}
}

func (h *StudioRunsHandler) runs(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	before, beforeErr := optionalInt(query.Get("before"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 50)
	if beforeErr != nil || limitErr != nil {
		return nil, badRequest("before and limit must be integers")
	}
	runs, err := h.Backend.Runs(ctx, actor, domain.AgentRunFilter{
		AgentID: query.Get("agent_name"), Status: domain.RunStatus(query.Get("status")), Before: before, Limit: int(limit),
	})
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentRun, 0, len(runs))
	for _, run := range runs {
		items = append(items, agentRunItem(run))
	}
	return items, nil
}

func (h *StudioRunsHandler) trace(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	runID, err := strconv.ParseInt(query.Get("run_id"), 10, 64)
	if err != nil || runID <= 0 {
		return nil, badRequest("run_id must be a positive integer")
	}
	trace, err := h.Backend.Trace(ctx, actor, query.Get("agent_name"), runID)
	if err != nil {
		return nil, err
	}
	return runTraceItem(trace), nil
}

func agentRunItem(run domain.AgentRun) api.AgentRun {
	return api.AgentRun{
		ID: run.ID, Version: run.Release.Version, TaskKind: run.TaskKind, RequestID: run.RequestID,
		Status: string(run.Status), CompletedAt: run.CompletedAt,
	}
}

func optionalInt(raw string, fallback int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
