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
	if config.Capacity <= 0 || config.TTL <= 0 {
		return fmt.Errorf("%s memory cache: capacity and ttl must be positive", name)
	}
	return nil
}
