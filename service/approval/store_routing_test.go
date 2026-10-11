package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestOpenRoundTripsTheRoutedAuthority(t *testing.T) {
	_, store := newApprovalStore(false)
	opened, err := store.Open(context.Background(), routedOwed("turn-1", "77", 1))
	if err != nil {
		t.Fatal(err)
	}
	want := routedOwed("turn-1", "77", 1)
	if opened.ReleaseVersion != "3" || opened.Requester != want.Requester || opened.Source != domain.ApprovalSourcePolicy ||
		opened.Route != want.Route || opened.Approver.ID != "77" {
		t.Fatalf("opened = %+v", opened)
	}
	unrouted, err := store.Open(context.Background(), owed("turn-2", "alex"))
	if err != nil || unrouted.Route != (domain.ApprovalRouteAuthority{}) || unrouted.ReleaseVersion != "" ||
		unrouted.Requester != (domain.PrincipalRef{}) || unrouted.Source != domain.ApprovalSourceAutonomy || unrouted.RuleID != "autonomy" {
		t.Fatalf("a request routed by scope = %+v, %v", unrouted, err)
	}
}

func TestOpenRefusesAnInconsistentRoutedRequest(t *testing.T) {
	for name, mutate := range map[string]func(*domain.ApprovalRequest){
		"no release":        func(r *domain.ApprovalRequest) { r.ReleaseVersion = "" },
		"human release":     func(r *domain.ApprovalRequest) { r.Principal.Kind = domain.PrincipalHuman },
		"unknown kind":      func(r *domain.ApprovalRequest) { r.Route.Reference.Kind = "team" },
		"blank ref":         func(r *domain.ApprovalRequest) { r.Route.Reference.Ref = " " },
		"long ref":          func(r *domain.ApprovalRequest) { r.Route.Reference.Ref = strings.Repeat("r", 129) },
		"step beyond three": func(r *domain.ApprovalRequest) { r.Route.Step = 4 },
		"long resolution":   func(r *domain.ApprovalRequest) { r.Route.Resolution = strings.Repeat("x", 257) },
		"service approver": func(r *domain.ApprovalRequest) {
			r.Approver = domain.PrincipalRef{Kind: domain.PrincipalService, ID: "bot"}
		},
		"non-decimal person": func(r *domain.ApprovalRequest) { r.Approver.ID = "alex" },
		"half requester":     func(r *domain.ApprovalRequest) { r.Requester.ID = "" },
		"unknown source":     func(r *domain.ApprovalRequest) { r.Source = "magic" },
		"no source":          func(r *domain.ApprovalRequest) { r.Source = "" },
		"no rule":            func(r *domain.ApprovalRequest) { r.RuleID = " " },
		"long rule":          func(r *domain.ApprovalRequest) { r.RuleID = strings.Repeat("r", 81) },
		"half approver":      func(r *domain.ApprovalRequest) { r.Approver.Kind = "" },
		"unrouted authority": func(r *domain.ApprovalRequest) { r.Route.RouteID = "" },
	} {
		request := routedOwed("turn-1", "77", 0)
		mutate(&request)
		_, store := newApprovalStore(false)
		if _, err := store.Open(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: %v, want validation", name, err)
		}
	}
	_, store := newApprovalStore(false)
	if _, err := store.Open(context.Background(), routedOwed("turn-1", "", 0)); err != nil {
		t.Fatalf("a routed request may open vacant: %v", err)
	}
}

func TestResolveIsFencedOnTheAssignmentItWasDecidedAgainst(t *testing.T) {
	table, store := newApprovalStore(false)
	ctx := context.Background()
	request, err := store.Open(ctx, routedOwed("turn-1", "77", 1))
	if err != nil {
		t.Fatal(err)
	}
	verdict := domain.ApprovalVerdict{
		RequestKey: domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3}, Status: domain.ApprovalStatusApproved,
		Decider: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}, ProposedDigest: request.ProposedDigest,
		Assignment: domain.ApprovalFence{Step: 0, Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}},
	}
	if _, err := store.Resolve(ctx, verdict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale step: %v, want conflict", err)
	}
	if got := table.fences[0]; got[0] != 0 || got[1] != "human" || got[2] != "77" {
		t.Fatalf("fence args = %v", got)
	}
	verdict.Assignment.Approver.ID = "88"
	verdict.Assignment.Step = 1
	if _, err := store.Resolve(ctx, verdict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale approver: %v, want conflict", err)
	}
	verdict.Assignment.Approver.ID = "77"
	if resolved, err := store.Resolve(ctx, verdict); err != nil || resolved.Status != domain.ApprovalStatusApproved {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	verdict.Assignment.Step = 4
	if _, err := store.Resolve(ctx, verdict); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an impossible step: %v", err)
	}
}

func TestReassignIsACompareAndSwapOnTheAssignment(t *testing.T) {
	table, store := newApprovalStore(false)
	store.Now = func() time.Time { return gateNow }
	ctx := context.Background()
	key := domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3}
	request, err := store.Open(ctx, routedOwed("turn-1", "77", 0))
	if err != nil {
		t.Fatal(err)
	}
	step := domain.EscalationStep{
		Action: domain.EscalationReassign, Backup: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "88"},
		Extension: 2 * time.Hour, Route: routedAuthority(1),
	}
	if err := store.Reassign(ctx, request, step); err != nil {
		t.Fatal(err)
	}
	moved, _ := store.Get(ctx, key)
	if moved.Approver.ID != "88" || moved.Route.Step != 1 || moved.Status != domain.ApprovalStatusEscalated || !moved.DeadlineAt.Equal(gateNow.Add(2*time.Hour)) {
		t.Fatalf("moved = %+v", moved)
	}
	if args := table.escalations[0]; args[10] != 0 || args[11] != "human" || args[12] != "77" {
		t.Fatalf("fence args = %v", args[10:])
	}
	// A second sweep read the request before the first moved it; its swap must lose.
	if err := store.Reassign(ctx, request, step); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale reassignment: %v, want conflict", err)
	}
	hold := domain.EscalationStep{Action: domain.EscalationHold, Backup: moved.Approver, Route: moved.Route}
	if err := store.Reassign(ctx, moved, hold); err != nil {
		t.Fatal(err)
	}
	held, _ := store.Get(ctx, key)
	if !held.DeadlineAt.IsZero() || held.Approver.ID != "88" {
		t.Fatalf("a hold leaves no deadline: %+v", held)
	}
}

func TestReassignFillsAVacancyUnderItsDeadline(t *testing.T) {
	table, store := newApprovalStore(false)
	ctx := context.Background()
	request, err := store.Open(ctx, routedOwed("turn-1", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	fill := domain.EscalationStep{Action: domain.EscalationReassign, Backup: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}, KeepDeadline: true, Route: routedAuthority(0)}
	if err := store.Reassign(ctx, request, fill); err != nil {
		t.Fatal(err)
	}
	filled, _ := store.Get(ctx, domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3})
	if filled.Approver.ID != "77" || filled.Status != domain.ApprovalStatusPending || !filled.DeadlineAt.Equal(request.DeadlineAt) {
		t.Fatalf("filled = %+v", filled)
	}
	if args := table.escalations[0]; args[11] != nil || args[12] != nil {
		t.Fatalf("a vacancy is fenced on no approver: %v", args[10:])
	}
}

func TestReassignWritesNoDeadlineForAZeroExtension(t *testing.T) {
	_, store := newApprovalStore(false)
	ctx := context.Background()
	request, err := store.Open(ctx, owed("turn-1", "alex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reassign(ctx, request, domain.EscalationStep{Backup: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}); err != nil {
		t.Fatal(err)
	}
	moved, _ := store.Get(ctx, domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3})
	if !moved.DeadlineAt.IsZero() || moved.Approver.ID != "42" {
		t.Fatalf("moved = %+v, want the backup with no deadline", moved)
	}
}

func TestReassignNotifiesTheNewApproverThroughTheOutbox(t *testing.T) {
	table, store := newApprovalStore(true)
	ctx := context.Background()
	request, err := store.Open(ctx, routedOwed("turn-1", "77", 0))
	if err != nil || len(table.events) != 1 {
		t.Fatalf("open: %v, events %d", err, len(table.events))
	}
	step := domain.EscalationStep{Action: domain.EscalationReassign, Backup: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "88"}, Extension: time.Hour, Route: routedAuthority(1)}
	if err := store.Reassign(ctx, request, step); err != nil {
		t.Fatal(err)
	}
	if len(table.events) != 2 {
		t.Fatalf("events = %d, want the backup notified", len(table.events))
	}
	found := false
	for _, payload := range table.events {
		var notice approvalNotice
		if json.Unmarshal([]byte(payload), &notice) == nil && notice.RecipientID == "88" && !notice.DueAt.IsZero() {
			found = true
		}
	}
	if !found {
		t.Fatalf("no notice for the backup: %v", table.events)
	}
	moved, _ := store.Get(ctx, domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3})
	if err := store.Reassign(ctx, moved, domain.EscalationStep{Action: domain.EscalationVacate, Extension: time.Hour, Route: routedAuthority(2)}); err != nil || len(table.events) != 2 {
		t.Fatalf("a vacancy notifies nobody: %v, events %d", err, len(table.events))
	}
}

func TestReassignRefusesAStepItsActionDoesNotAllow(t *testing.T) {
	person := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "88"}
	routed := routedOwed("turn-1", "77", 1)
	for name, test := range map[string]struct {
		request domain.ApprovalRequest
		step    domain.EscalationStep
	}{
		"reassign to nobody":        {routed, domain.EscalationStep{Action: domain.EscalationReassign, Route: routedAuthority(2)}},
		"routed to a service":       {routed, domain.EscalationStep{Backup: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "bot"}, Route: routedAuthority(2)}},
		"vacate unrouted":           {owed("turn-1", "alex"), domain.EscalationStep{Action: domain.EscalationVacate}},
		"vacate to someone":         {routed, domain.EscalationStep{Action: domain.EscalationVacate, Backup: person, Route: routedAuthority(2)}},
		"hold to someone else":      {routed, domain.EscalationStep{Action: domain.EscalationHold, Backup: person, Route: routedAuthority(1)}},
		"hold at another step":      {routed, domain.EscalationStep{Action: domain.EscalationHold, Backup: routed.Approver, Route: routedAuthority(2)}},
		"step backwards":            {routed, domain.EscalationStep{Backup: person, Route: routedAuthority(0)}},
		"step beyond three":         {routed, domain.EscalationStep{Backup: person, Route: routedAuthority(4)}},
		"another route":             {routed, domain.EscalationStep{Backup: person, Route: domain.ApprovalRouteAuthority{RouteID: "other", Reference: routed.Route.Reference, Step: 2}}},
		"unrouted with a step":      {owed("turn-1", "alex"), domain.EscalationStep{Backup: person, Route: routedAuthority(1)}},
		"keep an assigned deadline": {routed, domain.EscalationStep{Backup: person, KeepDeadline: true, Route: routedAuthority(1)}},
		"wait is not a write":       {routed, domain.EscalationStep{Action: domain.EscalationWait}},
		"expire is not a write":     {routed, domain.EscalationStep{Action: domain.EscalationExpire}},
		"negative extension":        {routed, domain.EscalationStep{Backup: person, Extension: -time.Minute, Route: routedAuthority(2)}},
	} {
		table, store := newApprovalStore(false)
		if err := store.Reassign(context.Background(), test.request, test.step); !errors.Is(err, domain.ErrValidation) || len(table.escalations) != 0 {
			t.Errorf("%s: %v, writes %d", name, err, len(table.escalations))
		}
	}
}

func TestVacantAsksForOpenRoutedRequestsNotYetDue(t *testing.T) {
	table, store := newApprovalStore(false)
	store.Now = func() time.Time { return gateNow }
	if _, err := store.Vacant(context.Background(), 7, 0); err != nil {
		t.Fatal(err)
	}
	if len(table.vacantArgs) != 3 || table.vacantArgs[0] != int64(7) || !table.vacantArgs[1].(time.Time).Equal(gateNow) || table.vacantArgs[2] != 100 {
		t.Fatalf("vacant args = %v", table.vacantArgs)
	}
}
