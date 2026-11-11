package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
)

// Playing without downloading, through Q2 Pod's URL queue ("urls" in its caps; q2-pod's
// patch/tidal.c app_urls_play): the app writes /tmp/q2-bandcamp.urls whole,
//
//	#Q2URLS/1<TAB>start=<index>
//	<url or card path><TAB><title><TAB><artist><TAB><album><TAB><cover url><TAB><seconds>
//
// and names it in /tmp/q2-bandcamp.play ("<seq> <path>"); Q2 Pod plays it with the player stock's
// Tidal uses, Now Playing showing each song's title, artist, album and cover. A song's lyrics, when
// Bandcamp has them, go in /tmp/q2-bandcamp.lyrics/<n>.lrc, n its place in the queue.
//
// Each URL is the app's own, on the Q2's loopback: http://127.0.0.1:<port>/t/<kind>/<list>/<track>.mp3
// for a song (kind and list: the release, playlist or show it was listed in), /b/<show>.mp3 for a
// radio show's broadcast, /i/<picture>.jpg for a cover. The proxy asks Bandcamp for the stream (its
// address fetched again when it is a day old, or refused) and passes Range requests through, so the
// player only sees plain HTTP from one host: no certificates, no redirects, no address that expires
// in a long-paused queue. A song already on the card is its card path instead.

const streamFresh = 12 * time.Hour // a stream address is used this long (Bandcamp gives a day)

var errNoURLs = errors.New("Playing needs a newer Q2 Pod (1.7 or later); downloads work")

type streamURL struct {
	url string
	at  time.Time
}

type streamer struct {
	d                  *daemon
	urls, play, lyrics string
	port               int
	srv                *http.Server

	mu      sync.Mutex
	streams map[int64]streamURL // a song's stream address, by track id
	pics    map[string][]byte   // the covers asked for last
	picKeys []string
	last    time.Time // the proxy's last use
	active  int       // requests being answered
}

func newStreamer(d *daemon, tmp string) *streamer {
	return &streamer{d: d, urls: tmp + ".urls", play: tmp + ".play", lyrics: tmp + ".lyrics",
		streams: map[int64]streamURL{}, pics: map[string][]byte{}}
}

// listen starts the proxy on the loopback, at port or the next free one.
func (s *streamer) listen(port int) error {
	var ln net.Listener
	var err error
	for p := port; p < port+20; p++ {
		if ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			s.port = ln.Addr().(*net.TCPAddr).Port // p, or the one given for 0 (tests)
			break
		}
	}
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return nil
}

func (s *streamer) close() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.srv.Shutdown(ctx)
		cancel()
		s.srv.Close()
	}
	os.Remove(s.play)
	os.Remove(s.urls)
	os.RemoveAll(s.lyrics)
}

// idle is how long the proxy has been unused (none while it answers).
func (s *streamer) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		return 0
	}
	if s.last.IsZero() {
		return time.Since(started)
	}
	return time.Since(s.last)
}

func (s *streamer) use(d int) {
	s.mu.Lock()
	s.active += d
	s.last = time.Now()
	s.mu.Unlock()
}

// keep notes the stream addresses a listing gave.
func (s *streamer) keep(ts []bandcamp.Track) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streams) > 5000 {
		s.streams = map[int64]streamURL{}
	}
	for _, t := range ts {
		if t.Stream != "" {
			s.streams[t.ID] = streamURL{t.Stream, time.Now()}
		}
	}
}

// entry is a song in a queue to play.
type entry struct {
	url, title, artist, album string
	pic                       bandcamp.Pic
	secs                      float64
	lyrics                    string
}

// trackURL is the proxy's address for a song listed in kind/list.
func (s *streamer) trackURL(kind string, list, id int64) string {
	return fmt.Sprintf("http://127.0.0.1:%d/t/%s/%d/%d.mp3", s.port, kind, list, id)
}

func (s *streamer) showURL(id int64) string {
	return fmt.Sprintf("http://127.0.0.1:%d/b/%d.mp3", s.port, id)
}

func (s *streamer) picURL(p bandcamp.Pic) string {
	if p.ID == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/i/%s.jpg", s.port, p.Key())
}

// field is s as a queue line's field: no tab or line break.
func field(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\t' || r == '\n' || r == '\r' }), " ")
}

// start has Q2 Pod play es from the from'th.
func (s *streamer) start(es []entry, from int) error {
	if !q2.HasCap("urls") {
		return errNoURLs
	}
	if s.srv == nil {
		return errors.New("the stream proxy isn't running")
	}
	if len(es) == 0 {
		return errors.New("nothing to play")
	}
	if from < 0 || from >= len(es) {
		from = 0
	}
	os.RemoveAll(s.lyrics)
	os.MkdirAll(s.lyrics, 0o755)
	var b strings.Builder
	fmt.Fprintf(&b, "#Q2URLS/1\tstart=%d\n", from)
	for i, e := range es {
		secs := ""
		if e.secs > 0 {
			secs = strconv.Itoa(int(e.secs + 0.5))
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", e.url, field(e.title), field(e.artist), field(e.album),
			s.picURL(e.pic), secs)
		if e.lyrics != "" {
			q2.WriteFile(filepath.Join(s.lyrics, strconv.Itoa(i)+".lrc"), []byte(e.lyrics))
		}
	}
	if err := q2.WriteFile(s.urls, []byte(b.String())); err != nil {
		return err
	}
	seq := strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := q2.WriteFile(s.play, []byte(seq+" "+s.urls+"\n")); err != nil {
		return err
	}
	s.use(0)
	logf("play: %d songs from %d (%s)", len(es), from, es[from].title)
	return nil
}

// ServeHTTP is the proxy.
func (s *streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.use(1)
	defer s.use(-1)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "t":
		list, e1 := strconv.ParseInt(parts[2], 10, 64)
		id, e2 := strconv.ParseInt(strings.TrimSuffix(parts[3], ".mp3"), 10, 64)
		if e1 != nil || e2 != nil {
			http.NotFound(w, r)
			return
		}
		s.proxy(w, r, func(ctx context.Context, renew bool) (string, error) { return s.song(ctx, parts[1], list, id, renew) })
	case len(parts) == 2 && parts[0] == "b":
		id, err := strconv.ParseInt(strings.TrimSuffix(parts[1], ".mp3"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.proxy(w, r, func(ctx context.Context, renew bool) (string, error) { return s.broadcast(ctx, id) })
	case len(parts) == 2 && parts[0] == "i":
		p, ok := bandcamp.ParsePic(strings.TrimSuffix(parts[1], ".jpg"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := s.picture(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// song is a song's stream address: the one kept, else its list's again from Bandcamp.
func (s *streamer) song(ctx context.Context, kind string, list, id int64, renew bool) (string, error) {
	s.mu.Lock()
	u, ok := s.streams[id]
	s.mu.Unlock()
	if ok && !renew && time.Since(u.at) < streamFresh {
		return u.url, nil
	}
	switch kind {
	case "album", "track", "playlist", "radio":
	default:
		return "", bandcamp.ErrNotFound
	}
	tl, err := s.d.bc.Tracklist(ctx, kind, list)
	if err != nil {
		return "", err
	}
	s.keep(tl.Tracks)
	s.mu.Lock()
	u, ok = s.streams[id]
	s.mu.Unlock()
	if !ok {
		return "", bandcamp.ErrNotFound
	}
	return u.url, nil
}

// broadcast is a radio show's whole broadcast (a stream_redirect, followed when it is opened).
func (s *streamer) broadcast(ctx context.Context, id int64) (string, error) {
	tl, err := s.d.cachedList(ctx, "radio", id)
	if err != nil {
		return "", err
	}
	if tl.Stream == "" {
		return "", bandcamp.ErrNotFound
	}
	return tl.Stream, nil
}

// proxy answers r with the stream resolve names, asking for a new address once if Bandcamp
// refuses the one it had.
func (s *streamer) proxy(w http.ResponseWriter, r *http.Request, resolve func(context.Context, bool) (string, error)) {
	ctx := r.Context()
	t0 := time.Now()
	u, err := resolve(ctx, false)
	var resp *http.Response
	if err == nil {
		resp, err = s.d.bc.Open(ctx, u, r.Header.Get("Range"))
		if errors.Is(err, bandcamp.ErrExpired) {
			if u, err = resolve(ctx, true); err == nil {
				resp, err = s.d.bc.Open(ctx, u, r.Header.Get("Range"))
			}
		}
	}
	if err != nil {
		var se *bandcamp.StatusError
		if errors.As(err, &se) && se.Code == http.StatusRequestedRangeNotSatisfiable {
			w.WriteHeader(se.Code)
			return
		}
		logf("proxy %s (range %q): %v", r.URL.Path, r.Header.Get("Range"), err)
		code := http.StatusBadGateway
		if errors.Is(err, bandcamp.ErrNotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("Content-Type", "audio/mpeg")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	n, err := s.copy(w, resp.Body)
	logf("proxy %s (range %q): %d, %d bytes in %.1f s%s", r.URL.Path, r.Header.Get("Range"), resp.StatusCode, n,
		time.Since(t0).Seconds(), errNote(err))
}

func errNote(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// copy is io.Copy, the proxy's use noted as it goes.
func (s *streamer) copy(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var n int64
	last := time.Now()
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			if _, err := w.Write(buf[:k]); err != nil {
				return n, err
			}
			n += int64(k)
			if time.Since(last) > 10*time.Second {
				s.use(0)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// picture is a cover at 700 px, the last few kept.
func (s *streamer) picture(ctx context.Context, p bandcamp.Pic) ([]byte, error) {
	k := p.Key()
	s.mu.Lock()
	b, ok := s.pics[k]
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := s.d.bc.Picture(ctx, p, 16)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.picKeys) >= 4 {
		delete(s.pics, s.picKeys[0])
		s.picKeys = s.picKeys[1:]
	}
	s.picKeys = append(s.picKeys, k)
	s.pics[k] = b
	s.mu.Unlock()
	return b, nil
}

// releaseEntries is a release's songs to play: the copy on the card where there is one.
func (s *streamer) releaseEntries(r *bandcamp.Release, dir string) []entry {
	s.keep(r.Tracks)
	of := len(r.Tracks) + r.Missing
	es := make([]entry, 0, len(r.Tracks))
	for _, t := range r.Tracks {
		e := entry{url: s.trackURL(r.Type, r.ID, t.ID), title: t.Title, artist: cmpOr(t.Artist, r.Artist), album: r.Title,
			pic: bandcamp.Pic{ID: r.Art, Art: true}, secs: t.Seconds, lyrics: t.Lyrics}
		if dir != "" {
			num := t.Num
			if r.Type == "track" {
				num, of = 1, 1
			}
			if p := filepath.Join(dir, songFile(num, of, t.Title)); exists(p) {
				e.url = p
			}
		}
		es = append(es, e)
	}
	return es
}

// listEntries is a playlist's or a show's songs to play, those Bandcamp streams.
func (s *streamer) listEntries(tl *bandcamp.Tracklist, dir string) ([]entry, []int) {
	s.keep(tl.Tracks)
	var es []entry
	var at []int // es' place of each track, -1 for one left out
	for i, t := range tl.Tracks {
		if t.Stream == "" {
			at = append(at, -1)
			continue
		}
		at = append(at, len(es))
		e := entry{url: s.trackURL(tl.Type, tl.ID, t.ID), title: t.Title, artist: t.Artist, album: cmpOr(t.Album, tl.Title),
			pic: bandcamp.Pic{ID: t.Art, Art: true}, secs: t.Seconds}
		if dir != "" {
			if p := filepath.Join(dir, songFile(i+1, len(tl.Tracks), t.Title)); exists(p) {
				e.url = p
			}
		}
		es = append(es, e)
	}
	return es, at
}

func exists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}
