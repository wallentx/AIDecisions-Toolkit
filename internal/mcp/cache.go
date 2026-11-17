package mcp

import (
	"context"
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
	runs     sync.WaitGroup
}

// flight is one shared execution. It runs while at least one caller waits on
// it; when the last waiter is canceled, the execution's context is canceled.
type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	outcome runOutcome
}

func newCache(now func() time.Time) *cache {
	return &cache{now: now, entries: map[string]runOutcome{}, inFlight: map[string]*flight{}}
}

// get returns a fresh cached outcome (cached true) unless refresh is set or
// the entry is older than ttl. Otherwise it joins an in-flight run of the
// same key or starts one. Only error-free outcomes are stored. A caller whose
// ctx ends stops waiting and receives a "cancelled" outcome.
func (c *cache) get(ctx context.Context, key string, ttl time.Duration, refresh bool, execute func(context.Context) runOutcome) (runOutcome, bool) {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && !refresh && ttl > 0 && c.now().Sub(entry.At) < ttl {
		c.mu.Unlock()
		return entry, true
	}
	running, ok := c.inFlight[key]
	if !ok {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		running = &flight{done: make(chan struct{}), cancel: cancel}
		c.inFlight[key] = running
		c.runs.Add(1)
		go func() {
			defer c.runs.Done()
			outcome := execute(runCtx)
			cancel()
			c.mu.Lock()
			if c.inFlight[key] == running {
				delete(c.inFlight, key)
			}
			if previous, ok := c.entries[key]; outcome.cacheable() && (!ok || !previous.At.After(outcome.At)) {
				c.entries[key] = outcome
			}
			running.outcome = outcome
			c.mu.Unlock()
			close(running.done)
		}()
	}
	running.waiters++
	c.mu.Unlock()

	select {
	case <-running.done:
		return running.outcome, false
	case <-ctx.Done():
	}
	c.mu.Lock()
	running.waiters--
	if running.waiters == 0 {
		// Nobody waits any more: stop the child, and let the next caller start
		// a fresh run instead of joining one that is being canceled.
		if c.inFlight[key] == running {
			delete(c.inFlight, key)
		}
		running.cancel()
	}
	c.mu.Unlock()
	return runOutcome{At: c.now().UTC(), Error: &CollectorError{"cancelled", "request was cancelled before the collector finished"}}, false
}

// wait blocks until every execution has finished. Executions without waiters
// are already canceled, so this returns once their children have exited.
func (c *cache) wait() { c.runs.Wait() }
