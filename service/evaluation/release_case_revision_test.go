package evaluation

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane"
)

func TestCaseReplayShowsADecidersRevisionBeforeItsObservation(t *testing.T) {
	journal := &dataplane.MemoryLoopJournal{}
	key := domain.LoopKey{TenantID: 7, RequestID: "case-1", ExecutionStepID: 3}
	tool := domain.ToolReference{ToolID: "shop.refund"}
	revised := []byte(`{"amount":5}`)
	entries := []domain.LoopEntry{
		{Kind: domain.LoopEntryModel, Model: &domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: "c1", Name: "shop_refund", Arguments: []byte(`{"amount":1000}`)}}}},
		{Kind: domain.LoopEntryApprovalPending, Tool: tool},
		{Kind: domain.LoopEntryRevision, Tool: tool, Observation: &domain.ModelToolObservation{CallID: "c1"},
			Revision: &domain.CallRevision{RevisesProposal: 9, Arguments: revised, ArgumentsDigest: dataplane.DigestBytes(revised)}},
		{Kind: domain.LoopEntryObservation, Tool: tool, Observation: &domain.ModelToolObservation{CallID: "c1", Output: []byte(`{"refunded":5}`)}},
	}
	for index, entry := range entries {
		entry.EntryNo = index + 1
		if _, err := journal.Append(context.Background(), key, entry); err != nil {
			t.Fatal(err)
		}
	}
	replay := &caseReplay{journal: journal, principal: domain.Principal{TenantID: 7}}
	if err := replay.record(context.Background(), "case-1", domain.ExecutionStep{ExecutionStepID: 3}); err != nil {
		t.Fatal(err)
	}
	got := replay.trajectory
	if len(got) != 4 || string(got[0].Payload) != `{"amount":1000}` || got[2].Kind != domain.TrajectoryPolicy ||
		got[2].Name != "shop.refund" || string(got[2].Payload) != `{"amount":5}` || got[3].Kind != domain.TrajectoryObservation {
		t.Fatalf("trajectory = %+v", got)
	}
}
