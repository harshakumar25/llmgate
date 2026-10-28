package cache

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// fakeRedis is an in-process server speaking enough RESP for GET, SET PX and
// PTTL, so the Redis tier is tested without a real Redis.
type fakeRedis struct {
	ln   net.Listener
	mu   sync.Mutex
	data map[string]string
	ttl  map[string]time.Time
	cmds []string
}

func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln, data: map[string]string{}, ttl: map[string]time.Time{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, args[0])
		var out string
		switch strings.ToUpper(args[0]) {
		case "GET":
			if v, ok := f.data[args[1]]; ok {
				out = fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
			} else {
				out = "$-1\r\n"
			}
		case "SET":
			f.data[args[1]] = args[2]
			if len(args) == 5 && strings.ToUpper(args[3]) == "PX" {
				ms, _ := strconv.Atoi(args[4])
				f.ttl[args[1]] = time.Now().Add(time.Duration(ms) * time.Millisecond)
			}
			out = "+OK\r\n"
		case "PTTL":
			if exp, ok := f.ttl[args[1]]; ok {
				out = fmt.Sprintf(":%d\r\n", time.Until(exp).Milliseconds())
			} else {
				out = ":-2\r\n"
			}
		default:
			out = "-ERR unknown command\r\n"
		}
		f.mu.Unlock()
		io.WriteString(c, out)
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, n)
	for i := range args {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

func (f *fakeRedis) count(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func TestRedisSharesAcrossReplicas(t *testing.T) {
	f := startFakeRedis(t)
	a := NewRedis(f.ln.Addr().String(), 100) // two gateway replicas
	b := NewRedis(f.ln.Addr().String(), 100)

	a.Set("k", &providers.Response{Content: "Paris", Usage: providers.Usage{TotalTokens: 3}}, time.Minute)
	got, ok := b.Get("k")
	if !ok || got.Content != "Paris" || got.Usage.TotalTokens != 3 {
		t.Fatalf("replica b got %+v, %v", got, ok)
	}
	if s := b.Stats(); s.RemoteHits != 1 || s.Hits != 1 || s.Misses != 0 {
		t.Fatalf("stats = %+v", s)
	}

	// The second read is served from b's local tier: no new GET.
	gets := f.count("GET")
	if _, ok := b.Get("k"); !ok || f.count("GET") != gets {
		t.Fatal("hot key should be answered locally")
	}
}

func TestRedisMissAndKeyPrefix(t *testing.T) {
	f := startFakeRedis(t)
	c := NewRedis(f.ln.Addr().String(), 10)
	if _, ok := c.Get("nope"); ok {
		t.Fatal("unexpected hit")
	}
	c.Set("k", &providers.Response{Content: "x"}, time.Minute)
	f.mu.Lock()
	_, stored := f.data[keyPrefix+"k"]
	f.mu.Unlock()
	if !stored {
		t.Fatal("key not stored under the llmgate prefix")
	}
}

func TestRedisFailsOpen(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now

	c := NewRedis(addr, 10)
	start := time.Now()
	c.Set("k", &providers.Response{Content: "x"}, time.Minute)
	if _, ok := c.Get("other"); ok {
		t.Fatal("hit with Redis down")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a dead Redis must not stall requests")
	}
	// The local tier still works.
	if _, ok := c.Get("k"); !ok {
		t.Fatal("local tier should still serve while Redis is down")
	}
	if c.Stats().RemoteErrors == 0 {
		t.Fatal("errors not counted")
	}
}

func TestEmptyAddrIsMemory(t *testing.T) {
	if _, ok := NewRedis("", 10).(*memCache); !ok {
		t.Fatal("empty addr should give the in-memory cache")
	}
}
