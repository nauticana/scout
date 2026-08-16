package modelgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestMemoryCapacitySnapshotSourceEvictsAndBounds(t *testing.T) {
	now := routerNow
	source, err := NewMemoryCapacitySnapshotSource(time.Minute, 2, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", Region: "eu", RouteID: "a", Healthy: true}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := source.SnapshotFor(ctx, domain.ModelSelection{Provider: "p", Model: "m", Region: "eu", RouteID: "a"})
	if err != nil || !ok || !stored.ObservedAt.Equal(now) || stored.Generation != 1 {
		t.Fatalf("snapshot = %+v ok=%t err=%v", stored, ok, err)
	}
	if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", RouteID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", RouteID: "c"}); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("expected route cap rejection, got %v", err)
	}

	now = now.Add(90 * time.Second)
	snapshots, err := source.Snapshots(ctx)
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("stale snapshots must be evicted: %+v %v", snapshots, err)
	}
	if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", RouteID: "c"}); err != nil {
		t.Fatal(err)
	}
	if snapshots, err = source.Snapshots(ctx); err != nil || len(snapshots) != 1 || snapshots[0].RouteID != "c" {
		t.Fatalf("snapshots = %+v %v", snapshots, err)
	}
}

func TestMemoryCapacitySnapshotSourceValidates(t *testing.T) {
	if _, err := NewMemoryCapacitySnapshotSource(0, 1, nil); err == nil {
		t.Fatal("expected retention error")
	}
	source, err := NewMemoryCapacitySnapshotSource(time.Minute, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Publish(context.Background(), domain.CapacitySnapshot{Model: "m"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
	if err := source.Publish(context.Background(), domain.CapacitySnapshot{Provider: "p", Model: "m", KVPressure: 2}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestMemoryCapacitySnapshotSourceOrdersAndTracksGeneration(t *testing.T) {
	source, err := NewMemoryCapacitySnapshotSource(time.Minute, 4, func() time.Time { return routerNow })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, route := range []string{"z", "a"} {
		if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", RouteID: route, Generation: 40}); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Publish(ctx, domain.CapacitySnapshot{Provider: "p", Model: "m", RouteID: "m"}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := source.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 3 || snapshots[0].RouteID != "a" || snapshots[2].RouteID != "z" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	if snapshots[1].Generation != 41 {
		t.Fatalf("unstamped snapshot must advance the local generation: %+v", snapshots[1])
	}
}
