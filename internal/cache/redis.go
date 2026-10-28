package cache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// redisCache is a two-tier cache: a local LRU in front of Redis. Replicas
// share completions through Redis, while hot keys are answered from local
// memory without a network round trip. It speaks just enough of the Redis
// protocol (RESP) for GET, SET and PTTL, so LLMGate keeps zero dependencies.
//
// It fails open: if Redis is slow or down, lookups count as misses and writes
// are dropped, and requests still go to the provider. A cache outage costs
// money, never availability.
type redisCache struct {
	addr    string
	prefix  string
	timeout time.Duration
	pool    chan *respConn
	local   *memCache

	remoteHits   atomic.Uint64
	remoteErrors atomic.Uint64
}

// keyPrefix namespaces LLMGate keys; bump the version when the stored
// Response shape changes so old entries are ignored rather than misread.
const keyPrefix = "llmgate:v1:"

// NewRedis returns a cache backed by Redis at addr (host:port), with a local
// LRU of maxEntries in front. An empty addr returns the in-memory cache.
func NewRedis(addr string, maxEntries int) Cache {
	if addr == "" {
		return NewMemory(maxEntries)
	}
	return &redisCache{
		addr:    addr,
		prefix:  keyPrefix,
		timeout: 250 * time.Millisecond,
		pool:    make(chan *respConn, 16),
		local:   NewMemory(maxEntries).(*memCache),
	}
}

func (c *redisCache) Get(key string) (*providers.Response, bool) {
	if resp, ok := c.local.Get(key); ok {
		return resp, true
	}
	// GET and PTTL go out in one round trip; the TTL lets the local copy
	// expire when the shared one does.
	replies, err := c.do([]string{"GET", c.prefix + key}, []string{"PTTL", c.prefix + key})
	if err != nil {
		c.fail("get", err)
		return nil, false
	}
	if replies[0].null {
		return nil, false
	}
	var resp providers.Response
	if err := json.Unmarshal(replies[0].data, &resp); err != nil {
		c.fail("decode", err)
		return nil, false
	}
	c.remoteHits.Add(1)
	if ttl := time.Duration(replies[1].n) * time.Millisecond; ttl > 0 {
		c.local.Set(key, &resp, ttl)
	}
	return &resp, true
}

func (c *redisCache) Set(key string, resp *providers.Response, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.local.Set(key, resp, ttl)
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	ms := strconv.FormatInt(ttl.Milliseconds(), 10)
	if _, err := c.do([]string{"SET", c.prefix + key, string(b), "PX", ms}); err != nil {
		c.fail("set", err)
	}
}

func (c *redisCache) Stats() Stats {
	s := c.local.Stats()
	remote := c.remoteHits.Load()
	s.Hits += remote
	s.Misses -= remote // a local miss that Redis answered is a hit overall
	s.RemoteHits = remote
	s.RemoteErrors = c.remoteErrors.Load()
	return s
}

func (c *redisCache) fail(op string, err error) {
	if c.remoteErrors.Add(1) == 1 {
		// Log the first failure loudly; after that /metrics carries the count.
		slog.Warn("redis cache unavailable, failing open", "op", op, "addr", c.addr, "err", err)
	}
}

// do sends cmds as one pipeline and reads one reply per command.
func (c *redisCache) do(cmds ...[]string) ([]reply, error) {
	conn, err := c.get()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	for _, cmd := range cmds {
		conn.writeCommand(cmd)
	}
	if err := conn.w.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	out := make([]reply, len(cmds))
	for i := range cmds {
		if out[i], err = conn.readReply(); err != nil {
			// The connection's position in the reply stream is now unknown.
			conn.Close()
			return nil, err
		}
	}
	c.put(conn)
	return out, nil
}

func (c *redisCache) get() (*respConn, error) {
	select {
	case conn := <-c.pool:
		return conn, nil
	default:
	}
	nc, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return nil, err
	}
	return &respConn{Conn: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}, nil
}

func (c *redisCache) put(conn *respConn) {
	select {
	case c.pool <- conn:
	default:
		conn.Close() // pool full
	}
}

// respConn is one Redis connection with buffered I/O.
type respConn struct {
	net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

// reply is a decoded RESP reply: a bulk/simple string in data, an integer in
// n, or null for a missing key.
type reply struct {
	data []byte
	n    int64
	null bool
}

func (c *respConn) writeCommand(args []string) {
	fmt.Fprintf(c.w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(c.w, "$%d\r\n%s\r\n", len(a), a)
	}
}

var errRedis = errors.New("redis")

func (c *respConn) readReply() (reply, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return reply{}, err
	}
	if len(line) < 3 {
		return reply{}, fmt.Errorf("%w: short reply %q", errRedis, line)
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+':
		return reply{data: []byte(body)}, nil
	case '-':
		return reply{}, fmt.Errorf("%w: %s", errRedis, body)
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		return reply{n: n}, err
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return reply{}, err
		}
		if n < 0 {
			return reply{null: true}, nil
		}
		buf := make([]byte, n+2) // payload + CRLF
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return reply{}, err
		}
		return reply{data: buf[:n]}, nil
	}
	return reply{}, fmt.Errorf("%w: unexpected reply type %q", errRedis, line[0])
}
