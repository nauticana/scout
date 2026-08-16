package knowledge

import (
	"context"
	"testing"
	"time"
)

func TestReconcilerReportsLagOrphansTombstones(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	query := &ingestQueryFake{rows: map[string][][]any{
		qReconcileActive:     {{"g2"}},
		qReconcileOldest:     {{now.Add(-90 * time.Second)}},
		qReconcileOrphans:    {{int64(4)}},
		qReconcileTombstones: {{int64(2)}},
	}}
	reconciler := &Reconciler{DB: ingestDBFake{query: query}, Now: func() time.Time { return now }}
	report, err := reconciler.Reconcile(context.Background(), 7, "kb")
	if err != nil {
		t.Fatal(err)
	}
	if report.ActiveVersion != "g2" || report.FreshnessLag != 90*time.Second || report.OrphanChunks != 4 || report.Tombstones != 2 || !report.CheckedAt.Equal(now) {
		t.Fatalf("report = %+v", report)
	}
	for _, name := range []string{qReconcileActive, qReconcileOldest, qReconcileOrphans, qReconcileTombstones} {
		if args := query.named(name)[0].args; args[0] != int64(7) || args[1] != "kb" {
			t.Fatalf("%s args = %v", name, args)
		}
	}
	query.rows[qReconcileOldest] = [][]any{{nil}}
	query.rows[qReconcileActive] = nil
	report, err = reconciler.Reconcile(context.Background(), 7, "kb")
	if err != nil || report.FreshnessLag != 0 || report.ActiveVersion != "" {
		t.Fatalf("idle report = %+v, %v", report, err)
	}
}
