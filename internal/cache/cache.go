// Package cache stores and retrieves completions keyed by request content, so
// identical requests skip the provider call. The default is an in-memory TTL
// cache; swap in a Redis-backed Cache (see NewRedis) for a shared, persistent
// layer across replicas.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// Cache stores completions. Implementations must be safe for concurrent use.
type Cache interface {
	Get(key string) (*providers.Response, bool)
	Set(key string, resp *providers.Response, ttl time.Duration)
}

// Key derives a stable cache key from a request. Identical messages and
// sampling settings produce the same key.
func Key(req providers.Request) string {
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type entry struct {
	resp    *providers.Response
	expires time.Time
}

type memCache struct {
	mu sync.RWMutex
	m  map[string]entry
}

// NewMemory returns an in-memory TTL cache.
func NewMemory() Cache {
	return &memCache{m: make(map[string]entry)}
}

func (c *memCache) Get(key string) (*providers.Response, bool) {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.resp, true
}

func (c *memCache) Set(key string, resp *providers.Response, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.m[key] = entry{resp: resp, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// NewRedis is the seam for a shared cache.
//
// TODO (Day 6): implement a Redis-backed Cache.
//   1. go get github.com/redis/go-redis/v9
//   2. Get: GET key -> json.Unmarshal into *providers.Response
//   3. Set: json.Marshal + SET key val EX int(ttl.Seconds())
//
// Until then this returns the in-memory cache so the app runs with or without
// Redis configured.
func NewRedis(addr string) Cache {
	if addr == "" {
		return NewMemory()
	}
	return NewMemory()
}
