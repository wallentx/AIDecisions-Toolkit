package mcp

import (
	"sync"
	"time"
)

// cache keeps the latest successful outcome per key for a TTL and coalesces
// concurrent runs of the same key into one execution. It never touches disk.
type cache struct {
	now      func() time.Time
	mu       sync.Mutex
	entries  map[string]runOutcome
	inFlight map[string]*flight
}

type flight struct {
	done    chan struct{}
	outcome runOutcome
}

func newCache(now func() time.Time) *cache {
	return &cache{now: now, entries: map[string]runOutcome{}, inFlight: map[string]*flight{}}
}

// get returns a fresh cached outcome (cached true) unless refresh is set or
// the entry is older than ttl. Otherwise it joins an in-flight run of the
// same key or starts one. Only error-free outcomes are stored.
func (c *cache) get(key string, ttl time.Duration, refresh bool, execute func() runOutcome) (runOutcome, bool) {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && !refresh && ttl > 0 && c.now().Sub(entry.At) < ttl {
		c.mu.Unlock()
		return entry, true
	}
	if running, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		<-running.done
		return running.outcome, false
	}
	running := &flight{done: make(chan struct{})}
	c.inFlight[key] = running
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if running.outcome.cacheable() {
			c.entries[key] = running.outcome
		}
		c.mu.Unlock()
		close(running.done)
	}()
	running.outcome = execute()
	return running.outcome, false
}
