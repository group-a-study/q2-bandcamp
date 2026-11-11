// q2-bandcamp is a Q2 Pod card app (contrib/shanlingq2/README) that brings Bandcamp to the Q2, no
// account needed: the editors' New and Notable picks, Bandcamp Daily's Album of the Day, featured
// and fans' playlists, Bandcamp Radio's shows, and search. A press on an album downloads it as
// bandcamp-dl does (the 128 kbps MP3s Bandcamp's own player streams, tagged, with the cover and
// lyrics) into Bandcamp/ on the card, where it plays as local music; Play/Pause held on it plays it
// at once instead. Playlists and radio shows can be played or downloaded, a download kept as an
// album of its own. Playing goes through Q2 Pod's URL queue (its "urls" cap) and a proxy on the
// Q2's loopback, so its player only ever sees plain HTTP. It runs from the card's .bandcamp folder,
// copied to /tmp first.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
	"github.com/group-a-study/q2-bandcamp/internal/tlsroots"
)

const version = "q2-v1.0"

// transport carries every request to Bandcamp: TLS checked against the app's own roots, as the
// firmware has none. Tests answer for Bandcamp instead.
var transport = func() http.RoundTripper {
	t := tlsroots.Client(0).Transport.(*http.Transport)
	t.ResponseHeaderTimeout = 30 * time.Second // a stream's body may take an hour; its answer mustn't
	return t
}

// idleFor is how long it stays with nothing to download, its page unused and nothing streamed,
// before it stops (Wi-Fi stays on while a card app runs).
var (
	idleFor       = 15 * time.Minute
	streamIdleFor = 30 * time.Minute
)

type daemon struct {
	dir     string // .bandcamp
	root    string // Bandcamp/
	tmp     string // /tmp/q2-bandcamp: its files for Q2 Pod are this with an extension
	bc      *bandcamp.Client
	state   *q2.State
	cache   *cache
	dl      *downloads
	st      *streamer
	art     *art
	browse  string
	control string

	mu    sync.Mutex
	used  time.Time
	known map[string]bandcamp.Album // releases the pages showed, by key, for a press's job
	quit  chan struct{}
	once  sync.Once
	stop  atomic.Bool
}

// proxyPort is the stream proxy's port on the loopback (the next free one if taken).
var proxyPort = 8117

func main() {
	dir := flag.String("dir", "/mnt/mmc/.bandcamp", "the app's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's mount point; downloads go to its Bandcamp folder")
	tmp := flag.String("tmp", "/tmp/q2-bandcamp", "the files shared with Q2 Pod: this with .state, .browse.sock, …")
	flag.IntVar(&proxyPort, "proxy-port", proxyPort, "the stream proxy's port, on the loopback")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(40 << 20)
	q2.Card = *card
	logPath = filepath.Join(*card, "q2-bandcamp.log")
	d := start(*dir, filepath.Join(*card, "Bandcamp"), *tmp)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { s := <-sig; logf("signal %v: stopping", s); d.shutdown() }()
	d.run()
	os.Exit(0)
}

func start(dir, root, tmp string) *daemon {
	tlsroots.DateURL = "http://bandcamp.com/"
	d := &daemon{dir: dir, root: root, tmp: tmp, bc: bandcamp.New(transport()), state: &q2.State{Path: tmp + ".state"},
		cache: newCache(), browse: tmp + ".browse.sock", control: tmp + ".sock", used: time.Now(),
		known: map[string]bandcamp.Album{}, quit: make(chan struct{})}
	os.MkdirAll(dir, 0o755)
	d.dl = newDownloads(d, root, dir)
	d.art = newArt(d, tmp+".art")
	d.st = newStreamer(d, tmp)
	if err := d.st.listen(proxyPort); err != nil {
		logf("stream proxy: %v", err)
	}
	d.writeState()
	q2.Control(d.control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			logf("Q2 Pod asked it to quit: stopping")
			d.shutdown()
		}
	})
	go q2spot.Serve(d.browse, d.device, d.quit)
	logStart(d)
	return d
}

// run downloads until asked to stop, writes the status file every second, and stops by itself
// once idle: nothing to download, its page unused, nothing streamed.
func (d *daemon) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.dl.run(ctx); close(done) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-d.quit:
			d.stop.Store(true)
			cancel()
			<-done // the song under way ended, its partial kept; the queue saved
			d.st.close()
			d.state.Remove()
			d.art.clear()
			os.Remove(d.browse)
			os.Remove(d.control)
			logf("stopped")
			return
		case <-t.C:
			d.writeState()
			d.mu.Lock()
			idle := time.Since(d.used)
			d.mu.Unlock()
			if !d.dl.pending() && idle > idleFor && d.st.idle() > streamIdleFor {
				logf("nothing to download, the page unused for %v and nothing streamed: stopping", idleFor)
				d.shutdown()
			}
			if n%60 == 0 {
				logMemory(d)
			}
			if n%300 == 0 {
				d.art.prune()
			}
		}
	}
}

func (d *daemon) shutdown() { d.once.Do(func() { close(d.quit) }) }

// running is whether it isn't stopping.
func (d *daemon) running() bool { return !d.stop.Load() }

func (d *daemon) touch() {
	d.mu.Lock()
	d.used = time.Now()
	d.mu.Unlock()
}

// writeState is what the Q2 reads: whether to hold standby off (a song downloading).
func (d *daemon) writeState() {
	hold := "0"
	if d.dl.busy() {
		hold = "1"
	}
	d.state.Write(map[string]string{"hold": hold, "version": version})
}

// askLibrary asks Q2 Pod for Update Local Music (addon_library.c), which it runs once nothing
// plays and no page of its own is open, so new downloads join Artists, Albums and Coverflow.
func (d *daemon) askLibrary() {
	if !q2.HasCap("library") {
		return
	}
	seq := strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := q2.WriteFile(d.tmp+".library", []byte(seq+"\nscan\n")); err != nil {
		logf("couldn't ask for Update Local Music: %v", err)
		return
	}
	logf("asked Q2 Pod for Update Local Music (%s)", seq)
}

// remember keeps the releases a page showed, so a press can name its job before Bandcamp is asked.
func (d *daemon) remember(as []bandcamp.Album) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.known) > 2000 {
		d.known = map[string]bandcamp.Album{}
	}
	for _, a := range as {
		d.known[releaseKey(a.URL)] = a
	}
}

func (d *daemon) lookup(page string) (bandcamp.Album, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.known[releaseKey(page)]
	return a, ok
}
