package modelgateway

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryCapacitySnapshotSource keeps the latest snapshot per route in process memory.
// Snapshots older than Retention are evicted on read so a silent route becomes unknown.
type MemoryCapacitySnapshotSource struct {
	Retention time.Duration
	MaxRoutes int
	Now       func() time.Time

	mu         sync.Mutex
	generation int64
	routes     map[routeKey]domain.CapacitySnapshot
}

var _ contract.CapacitySnapshotSource = (*MemoryCapacitySnapshotSource)(nil)
var _ contract.RouteSnapshotLookup = (*MemoryCapacitySnapshotSource)(nil)
var _ contract.CapacitySnapshotPublisher = (*MemoryCapacitySnapshotSource)(nil)

// NewMemoryCapacitySnapshotSource builds a bounded in-memory snapshot view.
func NewMemoryCapacitySnapshotSource(retention time.Duration, maxRoutes int, now func() time.Time) (*MemoryCapacitySnapshotSource, error) {
	if retention <= 0 || maxRoutes <= 0 {
		return nil, fmt.Errorf("capacity snapshot source: retention and max routes must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &MemoryCapacitySnapshotSource{Retention: retention, MaxRoutes: maxRoutes, Now: now, routes: make(map[routeKey]domain.CapacitySnapshot)}, nil
}

func (source *MemoryCapacitySnapshotSource) validate() error {
	if source.Retention <= 0 || source.MaxRoutes <= 0 {
		return fmt.Errorf("capacity snapshot source: retention and max routes must be positive")
	}
	if source.Now == nil {
		source.Now = time.Now
	}
	if source.routes == nil {
		source.routes = make(map[routeKey]domain.CapacitySnapshot)
	}
	return nil
}

// Publish replaces the route's snapshot; a zero ObservedAt is stamped now and a
// zero Generation receives the next local generation.
func (source *MemoryCapacitySnapshotSource) Publish(_ context.Context, snapshot domain.CapacitySnapshot) error {
	if !snapshotRouteKey(snapshot).valid() {
		return fmt.Errorf("%w: snapshot provider and model are required", domain.ErrValidation)
	}
	if snapshot.KVPressure < 0 || snapshot.KVPressure > 1 || snapshot.ServiceRate < 0 || snapshot.PrefillCapacity < 0 || snapshot.DecodeCapacity < 0 {
		return fmt.Errorf("%w: snapshot capacity figures are out of range", domain.ErrValidation)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := source.validate(); err != nil {
		return err
	}
	now := source.Now()
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = now
	}
	key := snapshotRouteKey(snapshot)
	if _, known := source.routes[key]; !known {
		source.evictLocked(now)
		if len(source.routes) >= source.MaxRoutes {
			return fmt.Errorf("%w: snapshot route capacity %d reached", domain.ErrRateLimited, source.MaxRoutes)
		}
	}
	if snapshot.Generation == 0 {
		source.generation++
		snapshot.Generation = source.generation
	} else if snapshot.Generation > source.generation {
		source.generation = snapshot.Generation
	}
	source.routes[key] = snapshot
	return nil
}

// Snapshots returns every retained snapshot in a stable route order.
func (source *MemoryCapacitySnapshotSource) Snapshots(context.Context) ([]domain.CapacitySnapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := source.validate(); err != nil {
		return nil, err
	}
	source.evictLocked(source.Now())
	snapshots := make([]domain.CapacitySnapshot, 0, len(source.routes))
	for _, snapshot := range source.routes {
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshotRouteKey(snapshots[i]).String() < snapshotRouteKey(snapshots[j]).String()
	})
	return snapshots, nil
}

// SnapshotFor returns the retained snapshot for one selection's route.
func (source *MemoryCapacitySnapshotSource) SnapshotFor(_ context.Context, selection domain.ModelSelection) (domain.CapacitySnapshot, bool, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := source.validate(); err != nil {
		return domain.CapacitySnapshot{}, false, err
	}
	source.evictLocked(source.Now())
	snapshot, ok := source.routes[selectionRouteKey(selection)]
	return snapshot, ok, nil
}

func (source *MemoryCapacitySnapshotSource) evictLocked(now time.Time) {
	for key, snapshot := range source.routes {
		if now.Sub(snapshot.ObservedAt) > source.Retention {
			delete(source.routes, key)
		}
	}
}
