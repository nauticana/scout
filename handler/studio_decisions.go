package handler

import (
	"context"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureDecisions is the Studio feature StudioDecisionsHandler serves.
const StudioFeatureDecisions = "decisions"

// StudioDecisionsHandler serves the tenant's decision records.
type StudioDecisionsHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioDecisionsBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioDecisionsHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureDecisions, Version: 1, Object: domain.AuditAuthorizationObject, Actions: []string{domain.AuditView}}
}

// Routes returns the decisions route, ?before=&limit=&category=&outcome=.
func (h *StudioDecisionsHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioDecisionsPath: query(&h.AbstractHandler, h.decisions)}
}

func (h *StudioDecisionsHandler) decisions(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	before, beforeErr := optionalInt(query.Get("before"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 0)
	if beforeErr != nil || limitErr != nil {
		return nil, badRequest("before and limit must be integers")
	}
	page, err := h.Backend.Decisions(ctx, actor, domain.DecisionQuery{
		Category: query.Get("category"), Outcome: domain.DecisionOutcome(query.Get("outcome")), Before: before, Limit: int(limit),
	})
	if err != nil {
		return nil, err
	}
	result := api.DecisionPage{Records: make([]api.DecisionRecord, 0, len(page.Records)), NextBefore: page.NextBefore}
	for _, record := range page.Records {
		result.Records = append(result.Records, decisionRecordItem(record))
	}
	return result, nil
}

func decisionRecordItem(record domain.DecisionRecord) api.DecisionRecord {
	item := api.DecisionRecord{
		ID: record.ID, Category: record.Category, Action: record.Action, Resource: record.Resource,
		Outcome: string(record.Outcome), Reason: record.Reason, Principal: principalItem(record.Principal),
		ClientRef: record.ClientRef, WorkRef: record.WorkRef, ScopeID: record.ScopeID, ReleaseVersion: record.ReleaseVersion,
		PolicyID: record.PolicyID, PolicyVersion: record.PolicyVersion, EvidenceDigest: record.Evidence.Digest,
		RequestID: record.RequestID, ConversationID: record.ConversationID, OccurredAt: record.OccurredAt,
	}
	for _, obligation := range record.Obligations {
		item.Obligations = append(item.Obligations, string(obligation))
	}
	if record.Authority.GrantID != "" {
		item.Authority = &api.DecisionAuthority{Subject: principalItem(record.Authority.Subject), GrantID: record.Authority.GrantID,
			Grantor: optionalPrincipal(record.Authority.Grantor)}
	}
	return item
}

func principalItem(ref domain.PrincipalRef) api.PrincipalRef {
	return api.PrincipalRef{Kind: string(ref.Kind), ID: ref.ID}
}
