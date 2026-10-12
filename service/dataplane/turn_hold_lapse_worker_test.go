package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

type lapserFunc func(context.Context, int) ([]domain.LapsedHold, error)

func (function lapserFunc) LapseHolds(ctx context.Context, batch int) ([]domain.LapsedHold, error) {
	return function(ctx, batch)
}

func TestTurnHoldLapseWorkerTakesItsIntervalFromLoadedSettings(t *testing.T) {
	settings := domain.OperatorSettings{}
	compose := func(context.Context, keelport.DatabaseRepository) (contract.TurnHoldLapser, contract.AuditSink, error) {
		return nil, nil, nil
	}
	w, err := NewTurnHoldLapseWorker("lapse", compose, func() domain.OperatorSettings { return settings }, func(context.Context, keelport.DatabaseRepository) error {
		settings.LapsePeriod = 45 * time.Second
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.LoadConfig(context.Background(), nil); err != nil || w.Interval != 45 {
		t.Fatalf("interval = %d, %v", w.Interval, err)
	}
	settings.LapsePeriod = 0
	if err = w.applyPeriod(); err != nil || w.Interval != int(DefaultHoldLapsePeriod/time.Second) {
		t.Fatalf("a zero period takes the default: %d, %v", w.Interval, err)
	}
	settings.LapsePeriod = 2 * time.Hour
	if err = w.applyPeriod(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a period beyond an hour = %v", err)
	}
	if _, err = NewTurnHoldLapseWorker("lapse", nil, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an incomplete worker = %v", err)
	}
	if queries := w.GetOLTPQueries(); queries[qOperatorLapse] == "" {
		t.Fatal("the worker prepares the lapse statement")
	}
}

// §8.7: one pass lapses every due hold a batch at a time and audits each turn_hold_lapsed under
// the turn's fence scope, attributed to the person whose lease lapsed.
func TestTurnHoldLapseWorkerSweepsInBatchesAndAudits(t *testing.T) {
	audit := &observability.MemoryAuditSink{}
	var batches []int
	due := 5
	lapser := lapserFunc(func(_ context.Context, batch int) ([]domain.LapsedHold, error) {
		batches = append(batches, batch)
		var holds []domain.LapsedHold
		for ; due > 0 && len(holds) < batch; due-- {
			holds = append(holds, domain.LapsedHold{TenantID: 7, RequestID: "request-" + string(rune('0'+due)), AgentID: "writer-a", Fence: 9,
				Holder: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}})
		}
		return holds, nil
	})
	composed := 0
	w, err := NewTurnHoldLapseWorker("lapse", func(context.Context, keelport.DatabaseRepository) (contract.TurnHoldLapser, contract.AuditSink, error) {
		composed++
		if composed == 1 {
			return nil, nil, errors.New("database not ready")
		}
		return lapser, audit, nil
	}, func() domain.OperatorSettings { return domain.OperatorSettings{LapseBatch: 2} }, func(context.Context, keelport.DatabaseRepository) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	journal := &healthJournal{}
	w.ProcessQueue(context.Background(), journal, nil, nil, nil)
	w.ProcessQueue(context.Background(), journal, nil, nil, nil)
	if len(batches) != 3 || batches[0] != 2 || len(journal.infos) != 1 || !strings.Contains(journal.infos[0], "5 turns") || len(journal.errors) != 1 {
		t.Fatalf("batches %v, infos %v, errors %v", batches, journal.infos, journal.errors)
	}
	records := audit.Request(7, "request-5")
	if len(records) != 1 || records[0].Category != observability.AuditCategoryTurnHoldLapsed || records[0].Principal.ID != "42" ||
		!strings.Contains(string(records[0].Payload), `"fence":9`) {
		t.Fatalf("lapse audit = %+v", records)
	}
	failing := &TurnHoldLapseWorker{Compose: func(context.Context, keelport.DatabaseRepository) (contract.TurnHoldLapser, contract.AuditSink, error) {
		return lapserFunc(func(context.Context, int) ([]domain.LapsedHold, error) { return nil, errors.New("down") }), nil, nil
	}, Settings: func() domain.OperatorSettings { return domain.OperatorSettings{} }}
	if lapsed, err := failing.Sweep(context.Background(), nil); err == nil || lapsed != 0 {
		t.Fatalf("a failed sweep = %d, %v", lapsed, err)
	}
}
