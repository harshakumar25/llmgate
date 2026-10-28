// Package flight coalesces concurrent identical calls: while one call for a key
// is in flight, later callers with the same key wait for its result instead of
// starting their own. In a gateway this turns a burst of identical prompts
// (a retry storm, a fan-out, a popular cached-but-expired question) into a
// single upstream call. It is a small, dependency-free take on
// golang.org/x/sync/singleflight.
package flight

import (
	"sync"
	"sync/atomic"
)

type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// Group deduplicates calls by key. The zero value is ready to use.
type Group[V any] struct {
	mu    sync.Mutex
	calls map[string]*call[V]

	leaders atomic.Uint64
	shared  atomic.Uint64
}

// Do runs fn once per key at a time. Callers that arrive while fn is running
// for the same key block and receive the same result; shared reports whether
// this caller got another caller's result.
func (g *Group[V]) Do(key string, fn func() (V, error)) (v V, err error, shared bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call[V])
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		g.shared.Add(1)
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &call[V]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()
	g.leaders.Add(1)

	// Release waiters and forget the key even if fn panics.
	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		c.wg.Done()
	}()
	c.val, c.err = fn()
	return c.val, c.err, false
}

// Stats returns how many calls ran fn (leaders) and how many reused a result
// already in flight (shared).
func (g *Group[V]) Stats() (leaders, shared uint64) {
	return g.leaders.Load(), g.shared.Load()
}
