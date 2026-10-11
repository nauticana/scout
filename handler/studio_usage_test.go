package handler

import (
	"context"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type usageBackendFunc func(context.Context, domain.StudioActor, string, time.Time, time.Time) ([]domain.UsageLine, error)

func (fn usageBackendFunc) Usage(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.UsageLine, error) {
	return fn(ctx, actor, agentID, from, to)
}

func TestStudioUsageReportsEachModelOnItsOwnLine(t *testing.T) {
	handler := &StudioUsageHandler{Backend: usageBackendFunc(func(_ context.Context, _ domain.StudioActor, agentID string, from, to time.Time) ([]domain.UsageLine, error) {
		if agentID != "writer-a" || !to.After(from) {
			t.Fatalf("agent %q window %v..%v", agentID, from, to)
		}
		return []domain.UsageLine{
			{Release: "4", Model: domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"}, Currency: "USD", Exponent: 2, Turns: 2, InputTokens: 30, CostMinorUnits: 9},
			{Release: "4", Currency: "USD", Exponent: 2, Turns: 1, ToolCalls: 2},
		}, nil
	})}
	query := url.Values{"agent_name": {"writer-a"}, "from": {"2026-09-01T00:00:00Z"}, "to": {"2026-09-02T00:00:00Z"}}
	response, err := handler.usage(context.Background(), domain.StudioActor{TenantID: 7}, query)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.AgentUsageLine{
		{Version: "4", Provider: "anthropic", Model: "claude", Currency: "USD", Exponent: 2, Turns: 2, InputTokens: 30, CostMinorUnits: 9},
		{Version: "4", Currency: "USD", Exponent: 2, Turns: 1, ToolCalls: 2},
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("response = %+v", response)
	}
	if _, err := handler.usage(context.Background(), domain.StudioActor{TenantID: 7}, url.Values{"agent_name": {"writer-a"}}); err == nil {
		t.Fatal("a report without a window must be refused")
	}
}
