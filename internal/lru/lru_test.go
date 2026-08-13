package lru

import (
	"testing"
	"time"
)

func TestCacheEvictsLeastRecentAndExpires(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	cache := New[string, int](2, clock)

	cache.Set("a", 1, time.Minute)
	cache.Set("b", 2, time.Minute)
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("a missing")
	}
	cache.Set("c", 3, 0)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("b should be evicted as least recent")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("a should be expired")
	}
	if v, ok := cache.Get("c"); !ok || v != 3 {
		t.Fatalf("c = %d, %v; want no-expiration entry", v, ok)
	}
}

func TestCacheSweepDropsExpiredFromTail(t *testing.T) {
	now := time.Unix(0, 0)
	cache := New[int, int](8, func() time.Time { return now })
	for i := range 4 {
		cache.Set(i, i, time.Second)
	}
	now = now.Add(2 * time.Second)
	if scanned := cache.Sweep(2); scanned != 2 {
		t.Fatalf("scanned = %d", scanned)
	}
	if cache.Len() != 2 {
		t.Fatalf("len = %d after partial sweep", cache.Len())
	}
	cache.Sweep(10)
	if cache.Len() != 0 {
		t.Fatalf("len = %d after full sweep", cache.Len())
	}
}

func TestShardedRoutesAndCloses(t *testing.T) {
	cache := NewSharded[string, string](4, 16, 0, 0, nil)
	cache.Set("k", "v", time.Minute)
	if v, ok := cache.Get("k"); !ok || v != "v" {
		t.Fatalf("get = %q, %v", v, ok)
	}
	cache.Delete("k")
	if cache.Len() != 0 {
		t.Fatalf("len = %d", cache.Len())
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal("second close must be idempotent:", err)
	}
}

func TestShardedSweeperReclaimsExpired(t *testing.T) {
	cache := NewSharded[int, int](2, 8, time.Millisecond, 8, nil)
	defer cache.Close()
	for i := range 8 {
		cache.Set(i, i, time.Millisecond)
	}
	deadline := time.Now().Add(time.Second)
	for cache.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cache.Len() != 0 {
		t.Fatalf("len = %d, sweeper did not reclaim", cache.Len())
	}
}
