package dataplane

import (
	"fmt"
	"io"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/internal/memcache"
)

// MemoryCacheConfig bounds one in-process cache.
type MemoryCacheConfig struct {
	Capacity int
	TTL      time.Duration
	// RemoteTTL is the TTL of the shared tier this local cache fronts; when
	// set, TTL may not exceed it so a local entry never outlives the remote one.
	RemoteTTL time.Duration
}

type MemorySessionCache interface {
	contract.HotSessionCache
	io.Closer
}

type MemoryGraphCache interface {
	contract.ExecutionGraphCache
	io.Closer
}

// NewMemorySessionCache builds a bounded session cache.
func NewMemorySessionCache(config MemoryCacheConfig) (MemorySessionCache, error) {
	if err := validateMemoryCacheConfig("session", config); err != nil {
		return nil, err
	}
	return &memcache.MemorySessionCache{Capacity: config.Capacity, TTL: config.TTL}, nil
}

// NewMemoryGraphCache builds a bounded execution-graph cache.
func NewMemoryGraphCache(config MemoryCacheConfig) (MemoryGraphCache, error) {
	if err := validateMemoryCacheConfig("graph", config); err != nil {
		return nil, err
	}
	return &memcache.MemoryGraphCache{Capacity: config.Capacity, TTL: config.TTL}, nil
}

func validateMemoryCacheConfig(name string, config MemoryCacheConfig) error {
	if config.Capacity <= 0 || config.TTL <= 0 || config.RemoteTTL < 0 {
		return fmt.Errorf("%s memory cache: capacity and ttl must be positive and remote ttl must not be negative", name)
	}
	if config.RemoteTTL > 0 && config.TTL > config.RemoteTTL {
		return fmt.Errorf("%s memory cache: local ttl %s exceeds remote ttl %s", name, config.TTL, config.RemoteTTL)
	}
	return nil
}
