package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/logger"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type healthJournal struct {
	logger.ApplicationLogger
	infos, errors []string
}

func (j *healthJournal) Info(line string)  { j.infos = append(j.infos, line) }
func (j *healthJournal) Error(line string) { j.errors = append(j.errors, line) }

func TestAgentHealthWorkerTakesItsIntervalFromLoadedSettings(t *testing.T) {
	settings := domain.AgentHealthSettings{}
	w, err := NewAgentHealthWorker("health", func(context.Context, keelport.DatabaseRepository) (*AgentHealth, error) { return nil, nil },
		func() domain.AgentHealthSettings { return settings }, func(context.Context, keelport.DatabaseRepository) error {
			settings.Period = 90 * time.Second
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.LoadConfig(context.Background(), nil); err != nil || w.Interval != 90 {
		t.Fatalf("interval = %d, err = %v", w.Interval, err)
	}
	settings.Period = 0
	if err = w.applyPeriod(); err != nil || w.Interval != int(DefaultHealthPeriod/time.Second) {
		t.Fatalf("a zero period takes the default: %d, %v", w.Interval, err)
	}
	settings.Period = time.Millisecond
	if err = w.applyPeriod(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a sub-second period must fail the worker start: %v", err)
	}
	if _, err = NewAgentHealthWorker("health", nil, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an incomplete worker must be refused: %v", err)
	}
}

func TestAgentHealthWorkerComposesOnceAndJournalsEachPass(t *testing.T) {
	query := evaluatorQuery(nil, [][]any{{"hard", int64(3)}}, nil)
	composed := 0
	compose := func(_ context.Context, db keelport.DatabaseRepository) (*AgentHealth, error) {
		composed++
		if composed == 1 {
			return nil, errors.New("scope repository not ready")
		}
		health := healthService(query, policyOf(`{"max_hard_safety_events":1}`))
		health.Notifier = fake.AgentAlertNotifierFunc(func(context.Context, domain.AgentAlert) error { return errors.New("pager down") })
		return health, nil
	}
	w, err := NewAgentHealthWorker("health", compose, func() domain.AgentHealthSettings { return domain.AgentHealthSettings{} },
		func(context.Context, keelport.DatabaseRepository) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	journal := &healthJournal{}
	w.ProcessQueue(context.Background(), journal, queueDBFake{query: query}, nil, nil)
	w.ProcessQueue(context.Background(), journal, queueDBFake{query: query}, nil, nil)
	w.ProcessQueue(context.Background(), journal, queueDBFake{query: query}, nil, nil)
	if composed != 2 || len(journal.infos) != 2 || !strings.Contains(journal.infos[0], "opened 1") || !strings.Contains(journal.infos[0], "undelivered 1") {
		t.Fatalf("composed = %d, infos = %v", composed, journal.infos)
	}
	if len(journal.errors) != 3 || !strings.Contains(journal.errors[0], "not ready") || !strings.Contains(journal.errors[1], "pager down") {
		t.Fatalf("errors = %v", journal.errors)
	}
	if queries := w.GetOLTPQueries(); queries[qHealthOpenAlert] == "" || queries[qHealthSaveState] == "" {
		t.Fatal("the worker prepares the health queries")
	}
}
