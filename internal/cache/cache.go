// Package cache stores and retrieves completions keyed by request content, so
// identical requests skip the provider call. The default is a bounded
// in-memory LRU with per-entry TTL; NewRedis adds a shared Redis tier behind
// it so replicas reuse each other's completions.
package cache

import (
	"container/list"
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
	Stats() Stats
}

// Stats are cumulative counters for /metrics.
type Stats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64 // dropped to stay under the size bound
	Expired   uint64 // dropped because their TTL passed
	Entries   int    // in local memory

	// Redis tier only.
	RemoteHits   uint64 // local misses answered by Redis
	RemoteErrors uint64 // Redis calls that failed (treated as misses)
}

// Key derives a stable cache key from a request. Identical messages and
// sampling settings produce the same key, whether or not the reply is streamed.
func Key(req providers.Request) string {
	req.Stream = false
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DefaultMaxEntries bounds the in-memory cache when no size is configured.
const DefaultMaxEntries = 10_000

type entry struct {
	key     string
	resp    *providers.Response
	expires time.Time
}

// memCache is an LRU: the list front is the most recently used entry, and the
// back is evicted first once the cache is full. Expired entries are dropped
// lazily when read and preferentially when making room.
type memCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time
	stats Stats
}

// NewMemory returns an in-memory LRU cache holding at most maxEntries
// completions. maxEntries <= 0 means DefaultMaxEntries.
func NewMemory(maxEntries int) Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &memCache{
		max:   maxEntries,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		now:   time.Now,
	}
}

func (c *memCache) Get(key string) (*providers.Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.stats.Misses++
		return nil, false
	}
	e := el.Value.(*entry)
	if c.now().After(e.expires) {
		c.remove(el)
		c.stats.Expired++
		c.stats.Misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.stats.Hits++
	return e.resp, true
}

func (c *memCache) Set(key string, resp *providers.Response, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(ttl)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		e.resp, e.expires = resp, expires
		c.ll.MoveToFront(el)
		return
	}
	for c.ll.Len() >= c.max {
		c.evictOne()
	}
	c.items[key] = c.ll.PushFront(&entry{key: key, resp: resp, expires: expires})
}

func (c *memCache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = c.ll.Len()
	return s
}

// evictOne drops the least recently used entry, unless an expired entry sits
// near the back, in which case that one goes first and no live entry is lost.
func (c *memCache) evictOne() {
	now := c.now()
	el := c.ll.Back()
	for i := 0; el != nil && i < 8; i, el = i+1, el.Prev() {
		if now.After(el.Value.(*entry).expires) {
			c.remove(el)
			c.stats.Expired++
			return
		}
	}
	c.remove(c.ll.Back())
	c.stats.Evictions++
}

func (c *memCache) remove(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}
