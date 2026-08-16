package dataplane

import (
	"context"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestMemoryCacheFactoriesBuildClosableCaches(t *testing.T) {
	sessions, err := NewMemorySessionCache(MemoryCacheConfig{Capacity: 8, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	if err := sessions.Put(context.Background(), 1, domain.SessionSnapshot{ConversationID: "c"}); err != nil {
		t.Fatal(err)
	}

	graphs, err := NewMemoryGraphCache(MemoryCacheConfig{Capacity: 8, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer graphs.Close()
	if err := graphs.Put(context.Background(), 1, domain.ExecutionGraph{AgentID: "a", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCacheFactoriesRejectMissingBounds(t *testing.T) {
	if _, err := NewMemorySessionCache(MemoryCacheConfig{}); err == nil {
		t.Fatal("invalid session cache config must fail")
	}
	if _, err := NewMemoryGraphCache(MemoryCacheConfig{}); err == nil {
		t.Fatal("invalid graph cache config must fail")
	}
}

func TestMemoryCacheFactoriesBoundLocalTTLByRemoteTTL(t *testing.T) {
	if _, err := NewMemorySessionCache(MemoryCacheConfig{Capacity: 8, TTL: 2 * time.Minute, RemoteTTL: time.Minute}); err == nil {
		t.Fatal("local ttl above remote ttl must fail")
	}
	if _, err := NewMemorySessionCache(MemoryCacheConfig{Capacity: 8, TTL: time.Minute, RemoteTTL: -1}); err == nil {
		t.Fatal("negative remote ttl must fail")
	}
	sessions, err := NewMemorySessionCache(MemoryCacheConfig{Capacity: 8, TTL: time.Minute, RemoteTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
}
