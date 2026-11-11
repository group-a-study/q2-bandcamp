package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
)

// Row covers ("art" in Q2 Pod's caps; q2-pod's patch/addon.c): a row's cover is a JPEG of at most
// 128 px at /tmp/q2-bandcamp.art/<key>.jpg, key the FNV-1a 32 of the row's URI, a tab and its
// context, as 8 lowercase hex digits. Bandcamp's 100 px size (_3) is fetched once a picture into
// img/ there and linked under each row's key. Q2 Pod draws a list with covers when its reply comes,
// so a page waits for its covers (artWait at most; the rest come for its next visit).

var artWait = 4 * time.Second

type art struct {
	d    *daemon
	dir  string
	mu   sync.Mutex
	busy map[string]chan struct{} // pictures being fetched, by key
	sem  chan struct{}
}

func newArt(d *daemon, dir string) *art {
	return &art{d: d, dir: dir, busy: map[string]chan struct{}{}, sem: make(chan struct{}, 4)}
}

func rowKey(r q2spot.Row) string {
	h := fnv.New32a()
	h.Write([]byte(r.URI + "\t" + r.Context))
	return fmt.Sprintf("%08x", h.Sum32())
}

// ensure puts each row's picture under its key, fetching the missing ones, waiting at most artWait.
func (a *art) ensure(rows []q2spot.Row, pics []bandcamp.Pic) {
	if !q2.HasCap("art") {
		return
	}
	img := filepath.Join(a.dir, "img")
	os.MkdirAll(img, 0o755)
	var waits []chan struct{}
	for i, r := range rows {
		if i >= len(pics) || pics[i].ID == 0 {
			continue
		}
		dst := filepath.Join(a.dir, rowKey(r)+".jpg")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		src := filepath.Join(img, pics[i].Key()+".jpg")
		if _, err := os.Stat(src); err == nil {
			os.Link(src, dst)
			continue
		}
		waits = append(waits, a.fetch(pics[i], src, dst))
	}
	deadline := time.After(artWait)
	for _, w := range waits {
		select {
		case <-w:
		case <-deadline:
			return
		}
	}
}

// fetch brings p to src and links it to dst; the channel closes when it has.
func (a *art) fetch(p bandcamp.Pic, src, dst string) chan struct{} {
	k := p.Key()
	a.mu.Lock()
	if ch, ok := a.busy[k]; ok {
		a.mu.Unlock()
		linked := make(chan struct{})
		go func() { <-ch; os.Link(src, dst); close(linked) }()
		return linked
	}
	ch := make(chan struct{})
	a.busy[k] = ch
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.busy, k)
			a.mu.Unlock()
			close(ch)
		}()
		a.sem <- struct{}{}
		defer func() { <-a.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b, err := a.d.bc.Picture(ctx, p, 3)
		if err != nil {
			return
		}
		if q2.WriteFile(src, b) == nil {
			os.Link(src, dst)
		}
	}()
	return ch
}

// prune keeps /tmp small: the newest 400 pictures, and links to those only.
func (a *art) prune() {
	img := filepath.Join(a.dir, "img")
	files, _ := os.ReadDir(img)
	if len(files) > 400 {
		type f struct {
			name string
			at   time.Time
		}
		var fs []f
		for _, e := range files {
			if fi, err := e.Info(); err == nil {
				fs = append(fs, f{e.Name(), fi.ModTime()})
			}
		}
		sort.Slice(fs, func(i, j int) bool { return fs[i].at.After(fs[j].at) })
		for _, x := range fs[min(400, len(fs)):] {
			os.Remove(filepath.Join(img, x.name))
		}
	}
	links, _ := os.ReadDir(a.dir)
	for _, e := range links {
		if fi, err := e.Info(); err == nil && !e.IsDir() {
			if st, ok := linkCount(fi); ok && st < 2 { // its picture was pruned
				os.Remove(filepath.Join(a.dir, e.Name()))
			}
		}
	}
}

// clear removes the covers, as the app stops (/tmp is the Q2's memory).
func (a *art) clear() { os.RemoveAll(a.dir) }
