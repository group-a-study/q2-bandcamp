package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The pages' lists, fetched from Bandcamp and kept a while (each its own time: the editors' picks
// change daily, a release's tracks hardly). A request waits at most answerIn for a fetch (the Q2
// gives up after 15 s, q2-pod's patch/addon.c), which carries on: the page says Loading and asks
// again ("refresh") until it's there. A list kept past its time is still shown while it is fetched
// again, unless a fresh one is asked for (Download the Top 10 takes the picks as they are now).

var answerIn = 8 * time.Second // tests shorten it

// errLoading is a list still being fetched after answerIn.
var errLoading = errors.New("still loading")

type cache struct {
	mu    sync.Mutex
	m     map[string]*entryC
	slots chan struct{} // fetches under way at most: the Q2's memory is small
}

type entryC struct {
	v    any
	at   time.Time
	err  error
	busy chan struct{}
}

func newCache() *cache { return &cache{m: map[string]*entryC{}, slots: make(chan struct{}, 3)} }

// peek is key's value if one is kept, however old, without fetching.
func (c *cache) peek(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[key]; e != nil && !e.at.IsZero() {
		return e.v, true
	}
	return nil, false
}

func (c *cache) drop(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// get is key's value: one kept younger than keep; else fetch's, waited for at most answerIn
// (errLoading after, the fetch carrying on); else, while fetch runs again, the one kept (unless
// fresh).
func (c *cache) get(key string, keep time.Duration, fresh bool, fetch func(context.Context) (any, error)) (any, error) {
	c.mu.Lock()
	e := c.m[key]
	if e == nil {
		if len(c.m) >= 48 {
			for k, old := range c.m {
				if old.busy == nil {
					delete(c.m, k)
				}
			}
		}
		e = &entryC{}
		c.m[key] = e
	}
	if !e.at.IsZero() && time.Since(e.at) < keep {
		defer c.mu.Unlock()
		return e.v, nil
	}
	if e.busy == nil {
		if err := e.err; err != nil { // it failed after its request stopped waiting: this one says so
			delete(c.m, key)
			c.mu.Unlock()
			return nil, err
		}
		e.busy = make(chan struct{})
		go c.run(key, e, fetch)
	}
	busy, kept, v := e.busy, !e.at.IsZero(), e.v
	c.mu.Unlock()
	if kept && !fresh {
		return v, nil
	}
	select {
	case <-busy:
	case <-time.After(answerIn):
		return nil, errLoading
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.at.IsZero() {
		if c.m[key] == e {
			delete(c.m, key) // told: the next request tries again
		}
		return nil, e.err
	}
	return e.v, nil
}

func (c *cache) run(key string, e *entryC, fetch func(context.Context) (any, error)) {
	c.slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t0 := time.Now()
	v, err := fetch(ctx)
	cancel()
	<-c.slots
	logf("fetched %s in %.1f s%s", key, time.Since(t0).Seconds(), errNote(err))
	c.mu.Lock()
	if err == nil {
		e.v, e.at, e.err = v, time.Now(), nil
	} else if e.at.IsZero() {
		e.err = err
	}
	close(e.busy)
	e.busy = nil
	c.mu.Unlock()
}
