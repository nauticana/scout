package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/logger"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type journalRecorder struct {
	logger.ApplicationLogger
	infos, errors []string
}

func (j *journalRecorder) Info(line string)  { j.infos = append(j.infos, line) }
func (j *journalRecorder) Error(line string) { j.errors = append(j.errors, line) }

func TestReleaseActivationWorkerTakesItsPollPeriodFromLoadedSettings(t *testing.T) {
	settings := domain.ReleaseActivationSettings{}
	loaded := false
	w, err := NewReleaseActivationWorker("activations", func(context.Context, keelport.DatabaseRepository) (*ReleaseActivations, error) {
		return nil, nil
	}, func() domain.ReleaseActivationSettings { return settings }, func(context.Context, keelport.DatabaseRepository) error {
		loaded = true
		settings = domain.ReleaseActivationSettings{Batch: 5, PollPeriod: 45 * time.Second, MaxAttempts: 3}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.LoadConfig(context.Background(), nil); err != nil || !loaded || w.Interval != 45 {
		t.Fatalf("interval = %d, loaded = %v, err = %v", w.Interval, loaded, err)
	}
	settings.PollPeriod = 0
	if err = w.applyPollPeriod(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a zero poll period must fail the worker start: %v", err)
	}
	if _, err = NewReleaseActivationWorker("activations", nil, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an incomplete worker must be refused: %v", err)
	}
}

func TestReleaseActivationWorkerComposesOnceAndJournalsEachPass(t *testing.T) {
	db := newScriptedDB()
	composed := 0
	compose := func(_ context.Context, database keelport.DatabaseRepository) (*ReleaseActivations, error) {
		composed++
		if composed == 1 {
			return nil, errors.New("principal resolver not ready")
		}
		return activationService(database.(*scriptedDB)), nil
	}
	w, err := NewReleaseActivationWorker("activations", compose, activationSettings, func(context.Context, keelport.DatabaseRepository) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	journal := &journalRecorder{}
	w.ProcessQueue(context.Background(), journal, db, nil, nil)
	if len(journal.errors) != 1 || !strings.Contains(journal.errors[0], "not ready") {
		t.Fatalf("errors = %v", journal.errors)
	}

	definition, digest := frozenDefinition(t)
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationStable, [][]any{{"4"}})
	db.script(qActivationDone, [][]any{{int64(41)}})
	w.ProcessQueue(context.Background(), journal, db, nil, nil)
	w.ProcessQueue(context.Background(), journal, db, nil, nil)
	if composed != 2 || len(journal.errors) != 1 || len(journal.infos) != 2 || !strings.Contains(journal.infos[0], "activated 1") {
		t.Fatalf("composed = %d, infos = %v, errors = %v", composed, journal.infos, journal.errors)
	}
	if queries := w.GetOLTPQueries(); queries[qActivationClaim] == "" || queries[qStudioAudit] == "" {
		t.Fatal("the worker must declare the activation queries")
	}
}
