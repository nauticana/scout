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

// StudioFeatureFeedback is the Studio feature StudioFeedbackHandler serves.
const StudioFeatureFeedback = "feedback"

// StudioFeedbackHandler serves the ratings an agent's releases received, never per person.
type StudioFeedbackHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioFeedbackBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioFeedbackHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureFeedback, Version: 1, Actions: []string{string(domain.StudioView)}}
}

// Routes returns the feedback route: ?agent_name=&from=&to= with RFC 3339 times.
func (h *StudioFeedbackHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioFeedbackPath: query(&h.AbstractHandler, h.feedback)}
}

func (h *StudioFeedbackHandler) feedback(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	from, fromErr := time.Parse(time.RFC3339, query.Get("from"))
	to, toErr := time.Parse(time.RFC3339, query.Get("to"))
	if fromErr != nil || toErr != nil {
		return nil, badRequest("from and to must be RFC 3339 times")
	}
	summaries, err := h.Backend.Feedback(ctx, actor, query.Get("agent_name"), from, to)
	if err != nil {
		return nil, err
	}
	items := make([]api.ReleaseFeedback, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, api.ReleaseFeedback{Version: summary.Release, Counts: summary.Counts})
	}
	return items, nil
}
