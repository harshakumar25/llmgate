package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

func newTestCache(max int) (*memCache, *time.Time) {
	c := NewMemory(max).(*memCache)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	return c, &now
}

func resp(s string) *providers.Response { return &providers.Response{Content: s} }

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := newTestCache(2)
	c.Set("a", resp("a"), time.Minute)
	c.Set("b", resp("b"), time.Minute)
	c.Get("a") // a is now most recent, so b goes next
	c.Set("c", resp("c"), time.Minute)

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
	if s := c.Stats(); s.Evictions != 1 || s.Entries != 2 {
		t.Fatalf("stats = %+v, want 1 eviction and 2 entries", s)
	}
}

func TestExpiredEntriesMiss(t *testing.T) {
	c, now := newTestCache(10)
	c.Set("a", resp("a"), time.Second)
	*now = now.Add(2 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry returned")
	}
	if s := c.Stats(); s.Expired != 1 || s.Entries != 0 || s.Misses != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestFullCachePrefersDroppingExpired(t *testing.T) {
	c, now := newTestCache(2)
	c.Set("old", resp("old"), time.Second)
	c.Set("live", resp("live"), time.Hour)
	c.Get("old") // old is most recent, but it is about to expire
	*now = now.Add(2 * time.Second)
	c.Set("new", resp("new"), time.Hour)

	if _, ok := c.Get("live"); !ok {
		t.Fatal("live entry was evicted while an expired one was available")
	}
	if s := c.Stats(); s.Evictions != 0 || s.Expired != 1 {
		t.Fatalf("stats = %+v, want 0 evictions, 1 expired", s)
	}
}

func TestSizeStaysBounded(t *testing.T) {
	c, _ := newTestCache(100)
	for i := 0; i < 10_000; i++ {
		c.Set(fmt.Sprint(i), resp("x"), time.Hour)
	}
	if n := c.Stats().Entries; n != 100 {
		t.Fatalf("entries = %d, want 100", n)
	}
}

func TestKeyDependsOnSettings(t *testing.T) {
	base := providers.Request{Messages: []providers.Message{{Role: "user", Content: "hi"}}}
	warm := base
	warm.Temperature = 0.7
	if Key(base) == Key(warm) {
		t.Fatal("temperature must change the cache key")
	}
	if Key(base) != Key(base) {
		t.Fatal("key is not stable")
	}
}

func BenchmarkGetParallel(b *testing.B) {
	c := NewMemory(1000)
	for i := 0; i < 1000; i++ {
		c.Set(fmt.Sprint(i), resp("x"), time.Hour)
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(fmt.Sprint(i % 1000))
			i++
		}
	})
}
