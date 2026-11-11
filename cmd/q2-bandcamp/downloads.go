package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/id3"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
)

// Downloads, as bandcamp-dl does them: a job is a release (album or track, by its page), a playlist
// or a radio show (by id). Its songs are fetched one at a time, each the 128 kbps MP3 Bandcamp's
// player streams, into .bandcamp/partial/<track id>.mp3 in 512 KB chunks (each opened, written,
// synced and closed, so the card is almost never busy), resumed with an HTTP Range after a break,
// then written tagged (id3) into its folder under Bandcamp/ and the partial file removed. A release
// goes into Artist/Year - Album; a playlist's or a radio show's songs are kept as an album of their
// own (Playlists/<name>, Radio/<date> - <series> - <show>), numbered in the list's order, each song
// with its own cover, the list's picture as cover.jpg and a .q2-playlist file, which Q2 Pod reads as
// "use the folder's picture for this album".
//
// A song already in its folder is passed over, so a job asked for again fetches only what's
// missing. A song that fails for its own reason is tried 3 more times (5, 10, 15 s apart), then left
// out and named in the job's failures; no network, the card away or full pause the queue instead,
// with no try used up. Jobs waiting and the one under way are kept in .bandcamp/queue.json; finished
// ones in .bandcamp/done.json, the record of everything downloaded (record.go), kept after an album
// is deleted from the card, which is how a list knows an album is on the card or was. When the
// queue empties after new songs came, Q2 Pod is asked for Update Local Music (its "library" cap),
// which it runs once nothing plays.

const (
	songRetries = 3
	chunk       = 512 << 10
	keepFree    = 100 << 20 // the card space left alone
	stallAfter  = 60 * time.Second
)

var retryGap = 5 * time.Second // tests shorten it

type job struct {
	Key      string    `json:"key"`
	Kind     string    `json:"kind"` // album, playlist, radio
	URL      string    `json:"url,omitempty"`
	ID       int64     `json:"id,omitempty"`
	Title    string    `json:"title"`
	Artist   string    `json:"artist,omitempty"` // a release's artist, a playlist's curator, a show's series
	Genre    string    `json:"genre,omitempty"`
	Pic      string    `json:"pic,omitempty"` // its cover's key (bandcamp.Pic), for the pages
	Added    time.Time `json:"added"`
	Dir      string    `json:"dir,omitempty"` // its folder under Bandcamp/, once known
	Total    int       `json:"total,omitempty"`
	Done     int       `json:"done,omitempty"`
	Missing  int       `json:"missing,omitempty"` // songs Bandcamp doesn't stream
	Failed   []string  `json:"failed,omitempty"`  // "Title: why"
	Err      string    `json:"err,omitempty"`     // why the job itself failed
	Finished time.Time `json:"finished,omitempty"`
	Deleted  time.Time `json:"deleted,omitempty"` // when its folder left the card (record.go)
}

// releaseKey is a release page's key: its host and path, so the same album from any list is one.
func releaseKey(page string) string {
	u, err := url.Parse(page)
	if err != nil || u.Host == "" {
		return "album:" + page
	}
	return "album:" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

func listKey(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }

type downloads struct {
	d       *daemon
	root    string // Bandcamp/ on the card
	qfile   string // .bandcamp/queue.json
	dfile   string // .bandcamp/done.json
	partial string // .bandcamp/partial

	mu      sync.Mutex
	queue   []*job          // waiting, the first one under way
	done    map[string]*job // finished, by key
	recent  []string        // keys finished since the start, newest first
	cancel  context.CancelFunc
	curKey  string
	song    string // the song under way: "Title"
	songN   int    // its place in the job
	got     int64  // its bytes so far, of size
	size    int64
	stalled string // why the queue waits
	fresh   bool   // songs came since the last library request
	wake    chan struct{}
}

func newDownloads(d *daemon, root, dir string) *downloads {
	dl := &downloads{d: d, root: root, qfile: filepath.Join(dir, "queue.json"), dfile: filepath.Join(dir, "done.json"),
		partial: filepath.Join(dir, "partial"), done: map[string]*job{}, wake: make(chan struct{}, 1)}
	if b, err := os.ReadFile(dl.qfile); err == nil {
		json.Unmarshal(b, &dl.queue)
	}
	if b, err := os.ReadFile(dl.dfile); err == nil {
		json.Unmarshal(b, &dl.done)
	}
	if dl.done == nil {
		dl.done = map[string]*job{}
	}
	// a removed job's song, half fetched, is kept to resume a few days, then let go
	parts, _ := os.ReadDir(dl.partial)
	for _, p := range parts {
		if fi, err := p.Info(); err == nil && time.Since(fi.ModTime()) > 72*time.Hour {
			os.Remove(filepath.Join(dl.partial, p.Name()))
		}
	}
	return dl
}

func (dl *downloads) poke() {
	select {
	case dl.wake <- struct{}{}:
	default:
	}
}

// saveQueue and saveDone write the files; the caller holds mu.
func (dl *downloads) saveQueue() {
	b, _ := json.Marshal(dl.queue)
	if err := q2.WriteFile(dl.qfile, b); err != nil {
		logf("couldn't save the queue: %v", err)
	}
}

func (dl *downloads) saveDone() {
	b, _ := json.Marshal(dl.done)
	if err := q2.WriteFile(dl.dfile, b); err != nil {
		logf("couldn't save the finished list: %v", err)
	}
	dl.writeRecord()
}

// state is what a key is doing: "" (nothing), "run", "wait", "done" (all of it on the card),
// "some" (on the card, songs failed), "gone" (downloaded, then deleted from the card), "fail" (the
// job failed, nothing downloaded); and its job.
func (dl *downloads) state(key string) (string, *job) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	for i, j := range dl.queue {
		if j.Key == key {
			if i == 0 && dl.curKey == key {
				return "run", j
			}
			return "wait", j
		}
	}
	j := dl.done[key]
	switch {
	case j == nil:
		return "", nil
	case j.Done == 0:
		if j.Err != "" {
			return "fail", j
		}
		return "", nil
	case !dl.onCard(j):
		return "gone", j
	case len(j.Failed) > 0:
		return "some", j
	}
	return "done", j
}

// onCard is whether a finished job's folder is there (the user may have deleted it).
func (dl *downloads) onCard(j *job) bool {
	if j.Dir == "" || j.Done == 0 {
		return false
	}
	fi, err := os.Stat(filepath.Join(dl.root, j.Dir))
	return err == nil && fi.IsDir()
}

// folder is a finished job's folder on the card, "" when it isn't there.
func (dl *downloads) folder(key string) string {
	if s, j := dl.state(key); s == "done" || s == "some" {
		return filepath.Join(dl.root, j.Dir)
	}
	return ""
}

// add queues j: "added", "queued" (it was already), "done" (on the card) or "gone" (downloaded
// before and deleted since) unless again: the record keeps a list's Download the Top N (and an
// artist's Download All) from fetching what was ever downloaded; a press on one row asks again.
func (dl *downloads) add(j *job, again bool) string {
	switch s, _ := dl.state(j.Key); {
	case s == "run" || s == "wait":
		return "queued"
	case (s == "done" || s == "some") && !again:
		return "done"
	case s == "gone" && !again:
		return "gone"
	}
	dl.mu.Lock()
	if old := dl.done[j.Key]; old != nil && old.Dir != "" {
		j.Dir = old.Dir
	}
	j.Added = time.Now()
	dl.queue = append(dl.queue, j)
	dl.saveQueue()
	dl.mu.Unlock()
	logf("queued %s %q (%s)", j.Kind, j.Title, j.Key)
	dl.poke()
	return "added"
}

// remove takes key off the queue, stopping it if it is under way (its partial song is kept).
func (dl *downloads) remove(key string) bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	for i, j := range dl.queue {
		if j.Key == key {
			if i == 0 && dl.curKey == key && dl.cancel != nil {
				dl.cancel()
			}
			dl.queue = append(dl.queue[:i], dl.queue[i+1:]...)
			dl.saveQueue()
			return true
		}
	}
	return false
}

// clear empties the queue but the job under way; how many were removed.
func (dl *downloads) clear() int {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	keep := dl.queue[:0]
	n := 0
	for i, j := range dl.queue {
		if i == 0 && dl.curKey == j.Key {
			keep = append(keep, j)
		} else {
			n++
		}
	}
	dl.queue = keep
	dl.saveQueue()
	return n
}

// forget drops finished jobs with nothing to try again from the Downloads page (not from the card).
func (dl *downloads) forget() {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	var keep []string
	for _, k := range dl.recent {
		if j := dl.done[k]; j != nil && (j.Err != "" || len(j.Failed) > 0) {
			keep = append(keep, k)
		}
	}
	dl.recent = keep
}

// pending is whether anything waits; busy whether a song is being fetched now.
func (dl *downloads) pending() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return len(dl.queue) > 0
}

func (dl *downloads) busy() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.curKey != "" && dl.stalled == ""
}

type snapshot struct {
	queue   []job
	cur     bool // queue[0] is under way
	song    string
	songN   int
	got     int64
	size    int64
	stalled string
	recent  []job // finished since the start, newest first, and older ones with failures
}

func (dl *downloads) snap() snapshot {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	s := snapshot{song: dl.song, songN: dl.songN, got: dl.got, size: dl.size, stalled: dl.stalled}
	for i, j := range dl.queue {
		s.queue = append(s.queue, *j)
		if i == 0 && dl.curKey == j.Key {
			s.cur = true
		}
	}
	seen := map[string]bool{}
	for _, k := range dl.recent {
		if j := dl.done[k]; j != nil && !seen[k] && (j.Done == 0 || j.Err != "" || dl.onCard(j)) {
			seen[k] = true
			s.recent = append(s.recent, *j)
		}
	}
	for k, j := range dl.done {
		if !seen[k] && (j.Err != "" || len(j.Failed) > 0) && dl.onCardOrErr(j) {
			s.recent = append(s.recent, *j)
		}
	}
	return s
}

func (dl *downloads) onCardOrErr(j *job) bool { return j.Err != "" || dl.onCard(j) }

// run downloads the queue until ctx ends.
func (dl *downloads) run(ctx context.Context) {
	dl.sweep()
	for ctx.Err() == nil {
		dl.mu.Lock()
		var j *job
		if len(dl.queue) > 0 {
			j = dl.queue[0]
		}
		fresh := dl.fresh
		if j == nil {
			dl.fresh = false
		}
		dl.mu.Unlock()
		if j == nil {
			if fresh {
				dl.d.askLibrary()
			}
			select {
			case <-ctx.Done():
				return
			case <-dl.wake:
			}
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		dl.mu.Lock()
		dl.cancel, dl.curKey = cancel, j.Key
		dl.mu.Unlock()
		start := time.Now()
		logf("job %s %q: start", j.Kind, j.Title)
		err := dl.runJob(jctx, j)
		cancel()
		dl.mu.Lock()
		dl.cancel, dl.curKey, dl.song, dl.got, dl.size, dl.stalled = nil, "", "", 0, 0, ""
		if ctx.Err() != nil { // stopping: it stays first in the queue, for the next start
			dl.saveQueue()
			dl.mu.Unlock()
			return
		}
		still := len(dl.queue) > 0 && dl.queue[0] == j
		if still { // not removed while it ran
			dl.queue = dl.queue[1:]
			if err != nil {
				j.Err = err.Error()
			} else {
				j.Err = ""
			}
			if old := dl.done[j.Key]; old != nil && old.Done > 0 && j.Done == 0 {
				// a deleted one asked for again that got nothing: the record keeps what it had,
				// and Downloads says why this try failed
				keep := *old
				keep.Err, keep.Failed = j.Err, j.Failed
				if keep.Err == "" {
					keep.Err = "none of its songs came"
				}
				j = &keep
			} else {
				j.Finished, j.Deleted = time.Now(), time.Time{}
			}
			dl.done[j.Key] = j
			dl.recent = append([]string{j.Key}, dl.recent...)
			if len(dl.recent) > 40 {
				dl.recent = dl.recent[:40]
			}
			dl.saveDone()
		}
		dl.saveQueue()
		dl.mu.Unlock()
		logf("job %s %q: %d of %d songs, %d failed, %d not streamed, err %v, %.0f s", j.Kind, j.Title, j.Done, j.Total,
			len(j.Failed), j.Missing, err, time.Since(start).Seconds())
	}
}

// stall notes why the queue waits ("" when it doesn't).
func (dl *downloads) stall(why string) {
	dl.mu.Lock()
	changed := dl.stalled != why
	dl.stalled = why
	dl.mu.Unlock()
	if changed && why != "" {
		logf("waiting: %s", why)
	}
}

// pause waits d, false when ctx ended first.
func pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ready waits for the card to be there with room on it, and says why while it isn't.
func (dl *downloads) ready(ctx context.Context) bool {
	for {
		switch {
		case !q2.CardMounted():
			dl.stall("The card is away")
		case freeBytes(q2.Card) < keepFree:
			dl.stall("The card is full")
		default:
			dl.stall("")
			return true
		}
		if !pause(ctx, 10*time.Second) {
			return false
		}
	}
}

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 1 << 40 // unknown: not in the way
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// errGone is a job whose page or list Bandcamp doesn't have any more.
var errGone = errors.New("Not on Bandcamp any more")

// song is one file of a plan.
type song struct {
	id      int64
	num, of int
	title   string
	artist  string
	stream  string
	lyrics  string
	url     string
	art     int64
}

// plan is a job made concrete: its folder, its tags and its songs.
type plan struct {
	dir         string // under Bandcamp/
	album       string
	albumArtist string
	year        string
	genre       string
	label       string
	url         string
	compilation bool
	cover       bandcamp.Pic // the folder's cover.jpg; and each song's, for a release
	ownCovers   bool         // a list's songs each carry their own release's cover
	marker      string       // .q2-playlist's text: a list kept as an album
	songs       []song
	missing     int
}

// plan asks Bandcamp for the job's songs, waiting out the network.
func (dl *downloads) plan(ctx context.Context, j *job) (*plan, error) {
	for try := 0; ; try++ {
		p, err := dl.makePlan(ctx, j)
		switch {
		case err == nil:
			dl.stall("")
			return p, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, bandcamp.ErrNotFound):
			return nil, errGone
		case bandcamp.IsNoNetwork(err):
			dl.stall("No connection to Bandcamp")
			try = 0
			if !pause(ctx, 15*time.Second) {
				return nil, ctx.Err()
			}
			continue
		case try >= songRetries:
			return nil, err
		}
		logf("job %q: %v; again", j.Title, err)
		if !pause(ctx, retryGap*time.Duration(try+1)) {
			return nil, ctx.Err()
		}
	}
}

func (dl *downloads) makePlan(ctx context.Context, j *job) (*plan, error) {
	bc := dl.d.bc
	switch j.Kind {
	case "album":
		r, err := bc.Release(ctx, j.URL)
		if err != nil {
			return nil, err
		}
		p := &plan{dir: albumDir(r.Artist, r.Title, r.Date), album: r.Title, albumArtist: r.Artist, label: r.Label,
			url: r.URL, cover: bandcamp.Pic{ID: r.Art, Art: true}, genre: j.Genre, missing: r.Missing}
		if !r.Date.IsZero() {
			p.year = r.Date.Format("2006")
		}
		if p.genre == "" && len(r.Tags) > 0 {
			p.genre = r.Tags[0]
		}
		of := len(r.Tracks) + r.Missing
		for _, t := range r.Tracks {
			p.songs = append(p.songs, song{id: t.ID, num: t.Num, of: of, title: t.Title, artist: cmpOr(t.Artist, r.Artist),
				stream: t.Stream, lyrics: t.Lyrics, url: cmpOr(t.URL, r.URL), art: r.Art})
		}
		if r.Type == "track" && len(p.songs) == 1 { // a track sold alone: its own one-song album
			p.songs[0].num, p.songs[0].of = 1, 1
		}
		return p, nil
	case "playlist", "radio":
		tl, err := bc.Tracklist(ctx, j.Kind, j.ID)
		if err != nil {
			return nil, err
		}
		p := &plan{compilation: true, ownCovers: true, cover: tl.Image}
		if j.Kind == "playlist" {
			p.dir, p.album, p.albumArtist = playlistDir(tl.Title), tl.Title, "Playlists"
			p.marker = fmt.Sprintf("bandcamp playlist %d\n%s\n", j.ID, tl.Title)
		} else {
			series := cmpOr(bandcamp.Series(tl.Subtitle), j.Artist)
			p.dir, p.album, p.albumArtist = showDir(series, tl.Title, tl.Date), joinNonEmpty(": ", series, tl.Title), "Bandcamp Radio"
			p.marker = fmt.Sprintf("bandcamp radio %d\n%s\n", j.ID, p.album)
		}
		if !tl.Date.IsZero() {
			p.year = tl.Date.Format("2006")
		}
		of := len(tl.Tracks)
		for i, t := range tl.Tracks {
			if t.Stream == "" {
				p.missing++
				continue
			}
			p.songs = append(p.songs, song{id: t.ID, num: i + 1, of: of, title: t.Title, artist: t.Artist, stream: t.Stream,
				url: cmpOr(t.AlbumURL, t.URL), art: t.Art})
		}
		return p, nil
	}
	return nil, fmt.Errorf("unknown kind %q", j.Kind)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// runJob fetches the job's songs; an error is the job's own (it failed as a whole).
func (dl *downloads) runJob(ctx context.Context, j *job) error {
	if !dl.ready(ctx) {
		return ctx.Err()
	}
	p, err := dl.plan(ctx, j)
	if err != nil {
		return err
	}
	dir := filepath.Join(dl.root, p.dir)
	dl.mu.Lock()
	j.Dir, j.Total, j.Missing, j.Done, j.Failed = p.dir, len(p.songs)+p.missing, p.missing, 0, nil
	j.Title = cmpOr(p.album, j.Title) // as Bandcamp names it, for a job queued by its address alone
	if j.Kind == "album" {
		j.Artist = cmpOr(p.albumArtist, j.Artist)
	}
	if pic, ok := bandcamp.ParsePic(j.Pic); (!ok || pic.ID == 0) && p.cover.ID != 0 {
		j.Pic = p.cover.Key()
	}
	dl.saveQueue()
	dl.mu.Unlock()
	if len(p.songs) == 0 {
		return errors.New("Bandcamp streams none of its songs")
	}
	if !dl.ready(ctx) {
		return ctx.Err()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("couldn't make its folder: %v", err)
	}
	covers := newCovers(dl.d.bc)
	if p.cover.ID != 0 {
		if _, err := os.Stat(filepath.Join(dir, "cover.jpg")); err != nil {
			if b, err := covers.get(ctx, p.cover); err == nil {
				q2.WriteFile(filepath.Join(dir, "cover.jpg"), b)
			}
		}
	}
	if p.marker != "" {
		q2.WriteFile(filepath.Join(dir, ".q2-playlist"), []byte(p.marker))
	}
	for i := 0; i < len(p.songs); i++ {
		s := p.songs[i]
		path := filepath.Join(dir, songFile(s.num, s.of, s.title))
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			dl.count(j, "")
			continue
		}
		dl.mu.Lock()
		dl.song, dl.songN, dl.got, dl.size = s.title, i+1, 0, 0
		dl.mu.Unlock()
		for try := 0; ; try++ {
			if !dl.ready(ctx) {
				return ctx.Err()
			}
			before := dl.partSize(s.id)
			err := dl.fetchSong(ctx, s, path, p, covers)
			if err == nil {
				dl.stall("")
				dl.count(j, "")
				dl.mu.Lock()
				dl.fresh = true
				dl.mu.Unlock()
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if bandcamp.IsNoNetwork(err) && dl.partSize(s.id) > before { // it broke off mid-song: resume now
				logf("song %q (%d): broke off at %d bytes (%v); resuming", s.title, s.id, dl.partSize(s.id), err)
				try = -1
				if !pause(ctx, retryGap) {
					return ctx.Err()
				}
				continue
			}
			if errors.Is(err, q2.ErrNoCard) || bandcamp.IsNoNetwork(err) {
				if !errors.Is(err, q2.ErrNoCard) {
					dl.stall("No connection to Bandcamp")
				}
				try = -1 // no try used: the song isn't to blame
				if !pause(ctx, 15*time.Second) {
					return ctx.Err()
				}
				continue
			}
			logf("song %q (%d): try %d: %v", s.title, s.id, try+1, err)
			if errors.Is(err, bandcamp.ErrExpired) { // a new list, for new stream addresses
				if np, perr := dl.makePlan(ctx, j); perr == nil {
					for _, ns := range np.songs {
						if ns.id == s.id {
							p.songs[i].stream, s.stream = ns.stream, ns.stream
						}
					}
				}
			}
			if try >= songRetries {
				dl.count(j, s.title+": "+why(err))
				break
			}
			if !pause(ctx, retryGap*time.Duration(try+1)) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// count notes a song done ("") or failed (its title and why).
func (dl *downloads) count(j *job, failed string) {
	dl.mu.Lock()
	if failed == "" {
		j.Done++
	} else {
		j.Failed = append(j.Failed, failed)
	}
	dl.saveQueue()
	dl.mu.Unlock()
}

// why is an error as the Q2 says it.
func why(err error) string {
	switch {
	case errors.Is(err, bandcamp.ErrExpired), errors.Is(err, bandcamp.ErrNotFound):
		return "Bandcamp doesn't send it"
	case errors.Is(err, errShort):
		return "it came cut short"
	}
	s := err.Error()
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

var errShort = errors.New("the song came cut short")

// fetchSong downloads s into its partial file (resuming one there), then writes it tagged at path.
func (dl *downloads) fetchSong(ctx context.Context, s song, path string, p *plan, covers *covers) error {
	if err := os.MkdirAll(dl.partial, 0o755); err != nil {
		return err
	}
	part := dl.partPath(s.id)
	if err := dl.fetchPart(ctx, s.stream, part); err != nil {
		return err
	}
	pic := p.cover
	if p.ownCovers {
		pic = bandcamp.Pic{ID: s.art, Art: true}
	}
	var cover []byte
	if pic.ID != 0 {
		cover, _ = covers.get(ctx, pic) // a song without its cover beats none
	}
	tags := id3.Tags{Title: s.title, Artist: s.artist, AlbumArtist: p.albumArtist, Album: p.album, Track: s.num, Tracks: s.of,
		Year: p.year, Genre: p.genre, Label: p.label, URL: cmpOr(s.url, p.url), Lyrics: s.lyrics, Compilation: p.compilation,
		Cover: cover}
	if err := writeTagged(part, path, tags); err != nil {
		return err
	}
	os.Remove(part)
	return nil
}

func (dl *downloads) partPath(id int64) string {
	return filepath.Join(dl.partial, strconv.FormatInt(id, 10)+".mp3")
}

// partSize is how much of a song's partial file is there.
func (dl *downloads) partSize(id int64) int64 {
	if fi, err := os.Stat(dl.partPath(id)); err == nil {
		return fi.Size()
	}
	return 0
}

// fetchPart brings the stream into part, from where an earlier try left it.
func (dl *downloads) fetchPart(parent context.Context, stream, part string) error {
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	rng := ""
	if have > 0 {
		rng = fmt.Sprintf("bytes=%d-", have)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	resp, err := dl.d.bc.Open(ctx, stream, rng)
	if err != nil {
		var se *bandcamp.StatusError
		if errors.As(err, &se) && se.Code == 416 && have > 0 { // all of it was there already
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if resp.StatusCode == 206 {
		var a, b int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &a, &b, &total); err != nil || a != have {
			return fmt.Errorf("a resume Bandcamp answered %q", resp.Header.Get("Content-Range"))
		}
	} else {
		have = 0 // the whole file again
		if err := os.Truncate(part, 0); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	dl.mu.Lock()
	dl.got, dl.size = have, total
	dl.mu.Unlock()
	timer := time.AfterFunc(stallAfter, cancel) // no byte for this long: a dead connection
	defer timer.Stop()
	buf := make([]byte, chunk)
	off := have
	for {
		n, rerr := io.ReadFull(resp.Body, buf)
		if n > 0 {
			timer.Reset(stallAfter)
			if err := q2.Append(part, off, buf[:n]); err != nil {
				return err
			}
			off += int64(n)
			dl.mu.Lock()
			dl.got = off
			dl.mu.Unlock()
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			if ctx.Err() != nil { // the stall timer's
				return fmt.Errorf("no data for %v: %w", stallAfter, context.DeadlineExceeded)
			}
			return rerr
		}
	}
	if total > 0 && off != total {
		return fmt.Errorf("%w (%d of %d bytes): %w", errShort, off, total, io.ErrUnexpectedEOF)
	}
	return nil
}

// writeTagged writes the tag and part's audio to path, through path's .part, renamed into place.
func writeTagged(part, path string, tags id3.Tags) error {
	if !q2.CardMounted() {
		return q2.ErrNoCard
	}
	in, err := os.Open(part)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = id3.Write(out, in, tags)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// covers fetches a job's covers (700 px), keeping the last few: a playlist's songs come from many
// releases, often a few in a row from one.
type covers struct {
	bc   *bandcamp.Client
	keys []string
	m    map[string][]byte
}

func newCovers(bc *bandcamp.Client) *covers { return &covers{bc: bc, m: map[string][]byte{}} }

func (c *covers) get(ctx context.Context, p bandcamp.Pic) ([]byte, error) {
	k := p.Key()
	if b, ok := c.m[k]; ok {
		return b, nil
	}
	b, err := c.bc.Picture(ctx, p, 16)
	if err != nil {
		return nil, err
	}
	if len(c.keys) >= 6 {
		delete(c.m, c.keys[0])
		c.keys = c.keys[1:]
	}
	c.keys = append(c.keys, k)
	c.m[k] = b
	return b, nil
}
