package flight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentCallsShareOneExecution(t *testing.T) {
	var g Group[int]
	var runs atomic.Int32
	release := make(chan struct{})

	const n = 50
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _, _ := g.Do("k", func() (int, error) {
				runs.Add(1)
				<-release
				return 42, nil
			})
			results[i] = v
		}(i)
	}
	// Let every goroutine reach Do before the leader finishes.
	waitFor(t, func() bool { l, s := g.Stats(); return l+s == n })
	close(release)
	wg.Wait()

	if r := runs.Load(); r != 1 {
		t.Fatalf("fn ran %d times, want 1", r)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("result[%d] = %d", i, v)
		}
	}
	if l, s := g.Stats(); l != 1 || s != n-1 {
		t.Fatalf("stats = %d leaders, %d shared", l, s)
	}
}

func TestDifferentKeysRunIndependently(t *testing.T) {
	var g Group[string]
	a, _, sa := g.Do("a", func() (string, error) { return "A", nil })
	b, _, sb := g.Do("b", func() (string, error) { return "B", nil })
	if a != "A" || b != "B" || sa || sb {
		t.Fatalf("got %q %q shared=%v,%v", a, b, sa, sb)
	}
}

func TestErrorsAreSharedAndKeyIsReleased(t *testing.T) {
	var g Group[int]
	boom := errors.New("boom")
	if _, err, _ := g.Do("k", func() (int, error) { return 0, boom }); err != boom {
		t.Fatalf("err = %v", err)
	}
	// A failed call must not stick: the next call runs fn again.
	v, err, shared := g.Do("k", func() (int, error) { return 7, nil })
	if v != 7 || err != nil || shared {
		t.Fatalf("got %d %v shared=%v", v, err, shared)
	}
}

func TestPanicReleasesWaiters(t *testing.T) {
	var g Group[int]
	func() {
		defer func() { _ = recover() }()
		g.Do("k", func() (int, error) { panic("x") })
	}()
	done := make(chan struct{})
	go func() {
		g.Do("k", func() (int, error) { return 1, nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("key stayed locked after a panic")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
