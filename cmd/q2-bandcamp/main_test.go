package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
)

// fake is Bandcamp as the app sees it: the APIs, release pages, streams and pictures, from the
// shapes the real site answered with (2026-11-11).
type fake struct {
	*httptest.Server
	mu      sync.Mutex
	cutOnce map[int64]bool  // a stream that breaks off halfway the first time
	gone    map[string]bool // album pages taken down since
	hold    chan struct{}   // while set, streams wait for it to close
	gets    map[string]int  // requests by path
}

type falbum struct {
	slug, title, artist string
	id, art             int64
	tracks              []ftrack
	date                string
	label               string
}

type ftrack struct {
	id       int64
	title    string
	artist   string
	noStream bool
	lyrics   string
}

var albums = map[string]*falbum{}

func init() {
	for n := 0; n < 12; n++ {
		slug := fmt.Sprintf("a%d", n)
		albums[slug] = &falbum{slug: slug, title: fmt.Sprintf("Album %d", n), artist: fmt.Sprintf("Artist %d", n),
			id: int64(1000 + n), art: int64(2000 + n), date: fmt.Sprintf("%02d Nov 2026 00:00:00 GMT", n+1),
			tracks: []ftrack{{id: int64(n*10 + 1), title: "One"}, {id: int64(n*10 + 2), title: "Two: Part/2?"}}}
	}
	albums["loves-arp"] = &falbum{slug: "loves-arp", title: "Love's Arp", artist: "Dawuna", id: 3848357538, art: 2440250561,
		date: "06 Nov 2026 00:00:00 GMT", label: "True Panther",
		tracks: []ftrack{{id: 501, title: "Love"}, {id: 502, title: "Love's Ekongo", lyrics: "first line\r\nsecond line"},
			{id: 503, title: "Unreleased", noStream: true}}}
}

// audio is a track's MP3 bytes: an MPEG frame header and LAME's Info tag, then the id's noise. 501
// is longer than two chunks.
func audio(id int64) []byte {
	n := 150<<10 + int(id%7)*1000
	if id == 501 {
		n = 1200 << 10
	}
	b := make([]byte, n)
	rand.New(rand.NewSource(id)).Read(b)
	copy(b, []byte{0xff, 0xfb, 0x90, 0x64, 'I', 'n', 'f', 'o'})
	return b
}

var jpeg = []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0, 0xff, 0xd9}

func newFake(t *testing.T) *fake {
	f := &fake{cutOnce: map[int64]bool{}, gone: map[string]bool{}, gets: map[string]int{}}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gets[r.URL.Path]++
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	bandcamp.Site, bandcamp.ImageHost = f.URL, f.URL
	api := func(path string, fn func(body map[string]any) any) {
		mux.HandleFunc("/api/"+path, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(fn(body))
		})
	}
	editorial := func(slugs []string, review bool) []any {
		var items []any
		for i, s := range slugs {
			a := albums[s]
			it := map[string]any{"date": a.date, "genre": "Electronic", "imageId": a.art, "itemId": a.id, "itemType": "album",
				"itemUrl": f.URL + "/album/" + s, "title": a.title, "description": "A blurb.",
				"attribution": map[string]any{"name": a.artist, "accountType": "band"},
				"playerData": map[string]any{"album": map[string]any{"id": a.id, "title": a.title, "artId": a.art,
					"url": f.URL + "/album/" + s}, "artistName": a.artist, "id": a.tracks[0].id}}
			if review {
				it["attribution"] = map[string]any{"name": "A Writer", "accountType": "contributor"}
				it["itemUrl"] = f.URL + "/daily/review-" + strconv.Itoa(i)
				it["title"] = a.artist + ", “" + a.title + "”"
			}
			items = append(items, it)
		}
		return items
	}
	var nn []string
	for n := 0; n < 12; n++ {
		nn = append(nn, fmt.Sprintf("a%d", n))
	}
	api("editorial/3/get_new_and_notable", func(map[string]any) any {
		return map[string]any{"items": editorial(nn, false), "nextCursor": nil}
	})
	api("editorial/3/get_aotd", func(map[string]any) any {
		return map[string]any{"items": editorial(append([]string{"loves-arp"}, nn[:9]...), true), "nextCursor": nil}
	})
	ptrack := func(t ftrack, a *falbum, at *float64) map[string]any {
		m := map[string]any{"id": t.id, "title": t.title, "artistName": cmpOr(t.artist, a.artist), "duration": 100.0,
			"artId": a.art, "url": f.URL + "/track/t" + strconv.FormatInt(t.id, 10), "isStreamingDisabled": t.noStream,
			"streamUrl": f.URL + "/stream/" + strconv.FormatInt(t.id, 10) + ".mp3",
			"album":     map[string]any{"id": a.id, "title": a.title, "artId": a.art, "url": f.URL + "/album/" + a.slug}}
		if at != nil {
			m["timecode"] = *at
		}
		return m
	}
	api("player/2/player_data_web", func(body map[string]any) any {
		kind, id := body["item_type"], int64(body["item_id"].(float64))
		var tracks []any
		tl := map[string]any{"itemType": kind, "itemId": id, "nextCursor": nil}
		switch {
		case kind == "playlist" && id == 77:
			tl["title"], tl["imageId"], tl["date"] = "Night Drive", 77000, "10 Nov 2026 03:53:49 GMT"
			tl["attribution"] = map[string]any{"name": "David James"}
			for _, s := range []string{"a1", "a2", "loves-arp", "a3"} {
				a := albums[s]
				tr := a.tracks[0]
				if s == "loves-arp" {
					tr = a.tracks[2] // not streamed
				}
				tracks = append(tracks, ptrack(tr, a, nil))
			}
		case kind == "radio" && id == 1018:
			tl["title"], tl["subtitle"], tl["imageId"], tl["date"] = "Robocobra Quartet", "Bandcamp Selects", 48231961, "09 Nov 2026 00:00:00 GMT"
			tl["compiledTrack"] = map[string]any{"streamUrl": f.URL + "/stream_redirect?id=9999", "duration": 5346.9}
			for i, s := range []string{"a4", "a5", "a6"} {
				at := float64(i * 190)
				tracks = append(tracks, ptrack(albums[s].tracks[1], albums[s], &at))
			}
		case kind == "album" || kind == "track":
			for _, a := range albums {
				if a.id == id {
					tl["title"] = a.title
					for _, t := range a.tracks {
						tracks = append(tracks, ptrack(t, a, nil))
					}
				}
			}
		default:
			return map[string]any{"__api_special__": "exception", "error_type": "Player::Tracklist::NotFoundError"}
		}
		tl["tracks"] = tracks
		return map[string]any{"tracklist": tl}
	})
	api("radio_api/1/get_radio_shows", func(map[string]any) any {
		items := []any{
			map[string]any{"itemId": 1018, "title": "Robocobra Quartet", "franchiseName": "Bandcamp Selects",
				"date": "09 Nov 2026 00:00:00 GMT", "imageId": 48231961, "description": "Belfast's jazz-punks."},
			map[string]any{"itemId": 1015, "title": "Enslaved", "franchiseName": "The Metal Show",
				"date": "06 Nov 2026 00:00:00 GMT", "imageId": 48231962},
			map[string]any{"itemId": 685, "title": "The End of Summer", "date": "06 Oct 2023 00:00:00 GMT", "imageId": 5},
		}
		for i := 0; i < 60; i++ {
			items = append(items, map[string]any{"itemId": 100 + i, "title": fmt.Sprintf("Weekly %d", i),
				"franchiseName": "Bandcamp Weekly", "date": "01 Jan 2020 00:00:00 GMT", "imageId": 7})
		}
		return map[string]any{"items": items, "nextCursor": nil}
	})
	mux.HandleFunc("/api/bcweekly/3/list", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": 685, "title": "The Hip-Hop Show"}}})
	})
	api("playlist/1/featured_playlists", func(map[string]any) any {
		return map[string]any{"items": []any{map[string]any{"itemId": 77, "title": "Night Drive", "imageId": 77000,
			"attribution": map[string]any{"name": "David James"}, "tracksSummary": map[string]any{"totalCount": 4}}},
			"nextCursor": nil}
	})
	api("fan_collection/1/playlists", func(body map[string]any) any {
		return map[string]any{"items": []any{map[string]any{"itemId": 77, "title": "Night Drive", "imageId": 77000,
			"attribution": map[string]any{"name": "David James"}, "tracksSummary": map[string]any{"totalCount": 4}}}}
	})
	api("bcsearch_public_api/1/autocomplete_elastic", func(body map[string]any) any {
		var r []any
		switch body["search_filter"] {
		case "a":
			r = append(r, map[string]any{"type": "a", "id": 3848357538, "name": "Love's Arp", "band_name": "Dawuna",
				"art_id": 2440250561, "item_url_path": f.URL + "/album/loves-arp"})
		case "b":
			r = append(r, map[string]any{"type": "b", "id": 9, "name": "Dawuna", "item_url_root": f.URL + "/band",
				"img_id": 26134068, "location": "Baltimore, Maryland"})
		case "f":
			r = append(r, map[string]any{"type": "f", "id": 613839, "name": "David James", "username": "funkentechno",
				"item_url_root": f.URL + "/funkentechno", "image_id": 37856494})
		}
		return map[string]any{"auto": map[string]any{"results": r}}
	})
	mux.HandleFunc("/album/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/album/")
		f.mu.Lock()
		down := f.gone[slug]
		f.mu.Unlock()
		a := albums[slug]
		if a == nil || down {
			http.NotFound(w, r)
			return
		}
		var info []any
		for i, t := range a.tracks {
			ti := map[string]any{"id": t.id, "track_id": t.id, "artist": nil, "title": t.title, "track_num": i + 1,
				"title_link": "/track/t" + strconv.FormatInt(t.id, 10), "duration": 100.5, "has_lyrics": t.lyrics != "",
				"lyrics": nil, "file": map[string]any{"mp3-128": f.URL + "/stream/" + strconv.FormatInt(t.id, 10) + ".mp3"}}
			if t.noStream {
				ti["file"] = nil
			}
			if t.lyrics != "" {
				ti["lyrics"] = t.lyrics
			}
			info = append(info, ti)
		}
		tralbum, _ := json.Marshal(map[string]any{"artist": a.artist, "item_type": "album", "id": a.id, "art_id": a.art,
			"url": f.URL + "/album/" + a.slug, "album_release_date": a.date, "current": map[string]any{"title": a.title},
			"trackinfo": info})
		ld, _ := json.Marshal(map[string]any{"@type": "MusicAlbum", "keywords": []string{"Jazz", "Baltimore"},
			"publisher": map[string]any{"name": cmpOr(a.label, a.artist)}})
		fmt.Fprintf(w, `<html><head><script type="application/ld+json">%s</script></head><body>
<script data-tralbum="%s" src="x.js"></script></body></html>`, ld, html.EscapeString(string(tralbum)))
	})
	mux.HandleFunc("/track/", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/track/t"), 10, 64)
		for _, a := range albums {
			for _, t := range a.tracks {
				if t.id == id {
					tralbum, _ := json.Marshal(map[string]any{"artist": a.artist, "item_type": "track", "id": id,
						"album_url": "/album/" + a.slug, "url": f.URL + r.URL.Path, "current": map[string]any{"title": t.title},
						"trackinfo": []any{map[string]any{"id": id, "title": t.title,
							"file": map[string]any{"mp3-128": f.URL + "/stream/" + strconv.FormatInt(id, 10) + ".mp3"}}}})
					fmt.Fprintf(w, `<div data-tralbum="%s"></div>`, html.EscapeString(string(tralbum)))
					return
				}
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/band/music", func(w http.ResponseWriter, r *http.Request) {
		items, _ := json.Marshal([]any{map[string]any{"id": 1002, "type": "album", "title": "Album 2", "page_url": "/album/a2",
			"art_id": 2002, "band_name": "Dawuna"}})
		fmt.Fprintf(w, `<p id="band-name-location"><span class="title">Dawuna</span></p>
<ol id="music-grid" data-client-items="%s">
<li data-item-id="album-1001" class="music-grid-item"><a href="/album/a1"><div class="art"><img src="%s/img/a0000002001_2.jpg"></div>
<p class="title">Album 1 <span class="artist-override">Guest</span></p></a></li></ol>`, html.EscapeString(string(items)), f.URL)
	})
	mux.HandleFunc("/funkentechno", func(w http.ResponseWriter, r *http.Request) {
		blob, _ := json.Marshal(map[string]any{"fan_data": map[string]any{"fan_id": 613839, "username": "funkentechno"}})
		fmt.Fprintf(w, `<div id="pagedata" data-blob="%s"></div>`, html.EscapeString(string(blob)))
	})
	mux.HandleFunc("/stream_redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/stream/"+r.URL.Query().Get("id")+".mp3", http.StatusFound)
	})
	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/stream/"), ".mp3")
		id, err := strconv.ParseInt(name, 10, 64)
		if err != nil {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		f.mu.Lock()
		cut, hold := f.cutOnce[id], f.hold
		delete(f.cutOnce, id)
		f.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		b := audio(id)
		w.Header().Set("Content-Type", "audio/mpeg")
		if cut && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(200)
			w.Write(b[:len(b)/2])
			panic(http.ErrAbortHandler) // the connection drops halfway
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) { w.Write(jpeg) })
	return f
}

func (f *fake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[path]
}

const allCaps = "playlist progress pages library goto sleep game audio sections art window urls sort"

type rig struct {
	t                    *testing.T
	f                    *fake
	d                    *daemon
	card, dir, tmp, caps string
	done                 chan struct{}
}

func newRig(t *testing.T, f *fake, base string) *rig {
	if base == "" {
		base = t.TempDir()
	}
	r := &rig{t: t, f: f, card: filepath.Join(base, "card"), tmp: filepath.Join(base, "t", "q2-bandcamp")}
	r.dir = filepath.Join(r.card, ".bandcamp")
	os.MkdirAll(r.dir, 0o755)
	os.MkdirAll(filepath.Dir(r.tmp), 0o755)
	r.caps = filepath.Join(base, "caps")
	if _, err := os.Stat(r.caps); err != nil {
		os.WriteFile(r.caps, []byte(allCaps+"\n"), 0o644)
	}
	q2.Card, q2.CapsFile = r.card, r.caps
	q2.CardMounted = func() bool { return true }
	transport = func() http.RoundTripper { return http.DefaultTransport }
	retryGap = 10 * time.Millisecond
	r.d = start(r.dir, filepath.Join(r.card, "Bandcamp"), r.tmp)
	r.done = make(chan struct{})
	go func() { r.d.run(); close(r.done) }()
	t.Cleanup(r.stop)
	return r
}

func (r *rig) stop() {
	r.d.shutdown()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.t.Error("the daemon didn't stop")
	}
}

func (r *rig) ask(op string, args ...string) q2spot.Reply {
	r.t.Helper()
	return r.d.device(q2spot.Request{ID: "1", Op: op, Args: args})
}

func wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *rig) idle() {
	r.t.Helper()
	wait(r.t, "the queue to empty", func() bool { return !r.d.dl.pending() })
}

// tags reads an MP3's ID3v2.4 frames and its audio.
func tags(t *testing.T, path string) (map[string]string, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:3]) != "ID3" || b[3] != 4 {
		t.Fatalf("%s: no v2.4 tag", path)
	}
	ss := func(x []byte) int { return int(x[0])<<21 | int(x[1])<<14 | int(x[2])<<7 | int(x[3]) }
	size := ss(b[6:10])
	body := b[10 : 10+size]
	out := map[string]string{}
	for len(body) >= 10 && body[0] != 0 {
		n := ss(body[4:8])
		v := body[10 : 10+n]
		if body[0] == 'T' {
			v = v[1:]
		}
		out[string(body[:4])] = string(v)
		body = body[10+n:]
	}
	return out, b[10+size:]
}

func rowsOf(rep q2spot.Reply) string {
	var b strings.Builder
	for _, r := range rep.Rows {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s\n", r.Kind, r.URI, r.Title, r.Subtitle, r.Context, r.Position)
	}
	return b.String()
}

func TestTopN(t *testing.T) {
	f := newFake(t)
	r := newRig(t, f, "")
	menu := r.ask("menu")
	if len(menu.Rows) < 6 || menu.Rows[0].URI != "nn" || menu.Rows[1].URI != "aotd" || menu.Rows[4].Kind != "search" ||
		menu.Rows[len(menu.Rows)-1].URI != "stop" {
		t.Fatalf("menu:\n%s", rowsOf(menu))
	}
	nn := r.ask("nn")
	if nn.Status != "" || len(nn.Rows) != 14 || nn.Rows[0].URI != "top" || nn.Rows[0].Context != "nn" ||
		nn.Rows[0].Title != "Download the Top 10" || nn.Rows[1].Kind != "list" || nn.Rows[1].URI != "topn" ||
		nn.Rows[1].Title != "How Many: 10" {
		t.Fatalf("nn:\n%s", rowsOf(nn))
	}
	a0 := nn.Rows[2]
	if a0.Kind != "action" || a0.URI != "get" || a0.Context != f.URL+"/album/a0" || a0.Position != "" ||
		a0.Title != "Album 0" || a0.Subtitle != "Artist 0 · Electronic" {
		t.Fatalf("an album's row: %+v", a0)
	}
	if _, err := os.Stat(filepath.Join(r.tmp+".art", rowKey(a0)+".jpg")); err != nil {
		t.Fatalf("its cover: %v", err)
	}
	// How Many, each list its own, 10 until chosen
	hm := r.ask("topn", "nn")
	if len(hm.Rows) != len(topChoices) || hm.Rows[3].Title != "Top 10" || hm.Rows[3].Subtitle != "Chosen · The first choice" ||
		hm.Rows[5].Context != "nn:20" || hm.Rows[5].URI != "setn" {
		t.Fatalf("how many:\n%s", rowsOf(hm))
	}
	if rep := r.ask("setn", "nn:3"); rep.Message != "Download the Top 3" {
		t.Fatalf("setn: %+v", rep)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "settings.json")); !strings.Contains(string(b), `"nn":3`) {
		t.Fatalf("settings.json: %s", b)
	}
	if nn = r.ask("nn"); nn.Rows[0].Title != "Download the Top 3" || nn.Rows[1].Title != "How Many: 3" {
		t.Fatalf("nn at 3:\n%s", rowsOf(nn))
	}
	if aotd := r.ask("aotd"); aotd.Rows[0].Title != "Download the Latest 10" || r.ask("topn", "aotd").Rows[0].Title != "Latest 1" {
		t.Fatalf("aotd's N isn't its own:\n%s", rowsOf(aotd))
	}
	if rep := r.ask("top", "nn"); rep.Message != "Downloading 3 albums" {
		t.Fatalf("top 3: %+v", rep)
	}
	if r.ask("nn").Message != "refresh" && r.d.dl.pending() {
		t.Fatal("the list isn't live while its albums download")
	}
	r.idle()
	if _, err := os.Stat(filepath.Join(r.card, "Bandcamp", "Artist 3")); err == nil {
		t.Fatal("the 4th was downloaded")
	}
	wait(t, "Update Local Music to be asked for", func() bool {
		b, err := os.ReadFile(r.tmp + ".library")
		return err == nil && strings.HasSuffix(string(b), "\nscan\n")
	})
	r.ask("setn", "nn:10")
	if rep := r.ask("top", "nn"); rep.Message != "Downloading 7 albums; passed over 3 on the card" {
		t.Fatalf("top 10: %+v", rep)
	}
	r.idle()
	for n := 0; n < 10; n++ {
		dir := filepath.Join(r.card, "Bandcamp", fmt.Sprintf("Artist %d", n), fmt.Sprintf("2026 - Album %d", n))
		tg, au := tags(t, filepath.Join(dir, "02 - Two_ Part_2_.mp3"))
		if tg["TIT2"] != "Two: Part/2?" || tg["TALB"] != fmt.Sprintf("Album %d", n) || tg["TPE1"] != fmt.Sprintf("Artist %d", n) ||
			tg["TPE2"] != fmt.Sprintf("Artist %d", n) || tg["TRCK"] != "2/2" || tg["TDRC"] != "2026" || tg["TCON"] != "Electronic" ||
			tg["WOAF"] != f.URL+"/track/t"+strconv.Itoa(n*10+2) || !strings.HasSuffix(tg["APIC"], string(jpeg)) {
			t.Fatalf("album %d's tags: %q", n, tg)
		}
		if !bytes.Equal(au, audio(int64(n*10+2))) {
			t.Fatalf("album %d's audio differs", n)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "cover.jpg")); err != nil || !bytes.Equal(b, jpeg) {
			t.Fatalf("album %d's cover.jpg: %v", n, err)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(r.dir, "partial")); len(left) != 0 {
		t.Fatalf("partial files left: %v", left)
	}
	nn = r.ask("nn")
	if nn.Rows[2].Kind != "local" || nn.Rows[2].Context != "album" ||
		nn.Rows[2].URI != filepath.Join(r.card, "Bandcamp", "Artist 0", "2026 - Album 0")+"/" ||
		nn.Rows[2].Position != "del:"+recID(releaseKey(f.URL+"/album/a0")) ||
		nn.Rows[0].Subtitle != "10 on the card" || nn.Rows[12].Kind != "action" || nn.Message != "" {
		t.Fatalf("nn after:\n%s", rowsOf(nn))
	}
	if rep := r.ask("top", "nn"); rep.Message != "Nothing new: 10 on the card" {
		t.Fatalf("top again: %+v", rep)
	}
	dls := r.ask("downloads")
	if !strings.Contains(rowsOf(dls), "|Clear Finished|") || dls.Rows[0].Kind != "local" || !strings.HasPrefix(dls.Rows[0].Position, "del:") {
		t.Fatalf("downloads:\n%s", rowsOf(dls))
	}
	// Album of the Day: the latest 10, the article's album (not the review) as each row's
	aotd := r.ask("aotd")
	if aotd.Rows[0].Title != "Download the Latest 10" || aotd.Rows[2].Context != f.URL+"/album/loves-arp" ||
		aotd.Rows[2].Subtitle != "Dawuna · 6 Nov" || aotd.Rows[3].Kind != "local" {
		t.Fatalf("aotd:\n%s", rowsOf(aotd))
	}
	if rep := r.ask("top", "aotd"); rep.Message != "Downloading 1 album; passed over 9 on the card" {
		t.Fatalf("latest 10: %+v", rep)
	}
	r.idle()
}

func TestRecordAndDelete(t *testing.T) {
	f := newFake(t)
	base := t.TempDir()
	r := newRig(t, f, base)
	r.ask("nn")
	r.ask("setn", "nn:3")
	r.ask("top", "nn")
	r.idle()
	key0, key1 := releaseKey(f.URL+"/album/a0"), releaseKey(f.URL+"/album/a1")
	id0, id1 := recID(key0), recID(key1)
	dir1 := filepath.Join(r.card, "Bandcamp", "Artist 1", "2026 - Album 1")
	record := func() string {
		b, _ := os.ReadFile(filepath.Join(r.card, "Bandcamp", recordName))
		return string(b)
	}
	if rec := record(); !strings.HasPrefix(rec, "Kind\tTitle\tArtist\tSongs\tDownloaded\tDeleted\tFolder\tLink\r\n") ||
		strings.Count(rec, "\r\n") != 4 || !strings.Contains(rec, "Album\tAlbum 1\tArtist 1\t2\t") ||
		!strings.Contains(rec, "\t\tArtist 1/2026 - Album 1\t"+f.URL+"/album/a1\r\n") {
		t.Fatalf("the record:\n%s", rec)
	}
	card := r.ask("card")
	if card.Rows[0].Title != "3 on the card · 0 deleted" || len(card.Rows) != 4 || card.Rows[1].URI != "rec" {
		t.Fatalf("downloaded:\n%s", rowsOf(card))
	}
	if m := r.ask("menu"); !strings.Contains(rowsOf(m), "|card|Downloaded|3 downloads on the card|") {
		t.Fatalf("menu:\n%s", rowsOf(m))
	}
	rp := r.ask("rec", id1)
	if rp.Rows[0].Kind != "local" || rp.Rows[0].URI != dir1+"/" || rp.Rows[1].URI != "delete" || rp.Rows[1].Context != id1 {
		t.Fatalf("its page:\n%s", rowsOf(rp))
	}
	// the first press asks, the second deletes
	if rep := r.ask("delete", id1); rep.Message != "Press again to delete “Album 1” from the card" {
		t.Fatalf("delete once: %+v", rep)
	}
	if _, err := os.Stat(dir1); err != nil {
		t.Fatal("deleted at the first press")
	}
	os.Remove(r.tmp + ".library")
	if rep := r.ask("delete", id1); rep.Message != "Deleted “Album 1” from the card" {
		t.Fatalf("delete twice: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(r.card, "Bandcamp", "Artist 1")); !os.IsNotExist(err) {
		t.Fatalf("its folders are still there: %v", err)
	}
	if _, err := os.Stat(r.tmp + ".library"); err != nil {
		t.Fatal("Update Local Music wasn't asked for after the delete")
	}
	if rec := record(); !regexp.MustCompile(`Album\tAlbum 1\tArtist 1\t2\t[0-9-]+ [0-9:]+\t[0-9-]+ [0-9:]+\t`).MatchString(rec) {
		t.Fatalf("the record after the delete:\n%s", rec)
	}
	if rp = r.ask("rec", id1); rp.Rows[0].URI != "redo" || !strings.HasPrefix(rp.Rows[0].Subtitle, "Deleted from the card ") {
		t.Fatalf("its page, deleted:\n%s", rowsOf(rp))
	}
	nn := r.ask("nn")
	if nn.Rows[3].Kind != "action" || nn.Rows[3].URI != "get" || !strings.HasPrefix(nn.Rows[3].Subtitle, "Deleted ") ||
		!strings.HasSuffix(nn.Rows[3].Subtitle, "press to download again") || nn.Rows[0].Subtitle != "2 on the card · 1 deleted" {
		t.Fatalf("nn after the delete:\n%s", rowsOf(nn))
	}
	// Download the Top N passes over what was deleted
	pages := f.count("/album/a1")
	r.ask("setn", "nn:4")
	if rep := r.ask("top", "nn"); rep.Message != "Downloading 1 album; passed over 2 on the card, 1 deleted before" {
		t.Fatalf("top 4: %+v", rep)
	}
	r.idle()
	if _, err := os.Stat(dir1); err == nil || f.count("/album/a1") != pages {
		t.Fatal("the deleted album came back")
	}
	if _, err := os.Stat(filepath.Join(r.card, "Bandcamp", "Artist 3", "2026 - Album 3", "01 - One.mp3")); err != nil {
		t.Fatal(err)
	}
	// and what was deleted by hand (File Browser) too
	os.RemoveAll(filepath.Join(r.card, "Bandcamp", "Artist 2"))
	if card = r.ask("card"); card.Rows[0].Title != "2 on the card · 2 deleted" {
		t.Fatalf("downloaded, one deleted by hand:\n%s", rowsOf(card))
	}
	if rep := r.ask("top", "nn"); rep.Message != "Nothing new: 2 on the card, 2 deleted before" {
		t.Fatalf("top after a delete by hand: %+v", rep)
	}
	// Play/Pause held twice on an album on the card deletes it
	row := r.ask("nn").Rows[2]
	if row.Position != "del:"+id0 {
		t.Fatalf("a0's row: %+v", row)
	}
	if rep := r.ask(row.Position, row.Context); rep.Message != "Hold Play/Pause again to delete “Album 0” from the card" {
		t.Fatalf("hold once: %+v", rep)
	}
	if rep := r.ask(row.Position, row.Context); rep.Message != "Deleted “Album 0” from the card" {
		t.Fatalf("hold twice: %+v", rep)
	}
	// a press on a deleted album's row downloads it again; so does Download Again on its page
	if rep := r.ask("get", f.URL+"/album/a1"); rep.Message != "Downloading “Album 1”" {
		t.Fatalf("get a deleted one: %+v", rep)
	}
	if rep := r.ask("redo", id0); rep.Message != "Downloading “Album 0” again" {
		t.Fatalf("redo: %+v", rep)
	}
	r.idle()
	if st, j := r.d.dl.state(key1); st != "done" || !j.Deleted.IsZero() {
		t.Fatalf("a1 again: %s %+v", st, j)
	}
	if st, _ := r.d.dl.state(key0); st != "done" {
		t.Fatalf("a0 again: %s", st)
	}
	// a deleted one asked for again that Bandcamp took down: the record keeps it, deleted, and
	// Downloads says why
	f.mu.Lock()
	f.gone["a2"] = true
	f.mu.Unlock()
	key2 := releaseKey(f.URL + "/album/a2")
	if rep := r.ask("get", f.URL+"/album/a2"); !strings.HasPrefix(rep.Message, "Downloading") {
		t.Fatalf("get a2: %+v", rep)
	}
	r.idle()
	if st, j := r.d.dl.state(key2); st != "gone" || j.Done != 2 || j.Err != errGone.Error() {
		t.Fatalf("a2 after a failed try: %s %+v", st, j)
	}
	if !strings.Contains(record(), "Album\tAlbum 2\tArtist 2\t2\t") {
		t.Fatalf("a2 left the record:\n%s", record())
	}
	if !strings.Contains(rowsOf(r.ask("downloads")), "|Album 2|Couldn't download: Not on Bandcamp any more · press to try again|") {
		t.Fatalf("downloads:\n%s", rowsOf(r.ask("downloads")))
	}
	if rep := r.ask("top", "nn"); rep.Message != "Nothing new: 3 on the card, 1 deleted before" {
		t.Fatalf("top after: %+v", rep)
	}
	// the record and the settings outlive a restart
	r.stop()
	r2 := newRig(t, f, base)
	if card = r2.ask("card"); card.Rows[0].Title != "3 on the card · 1 deleted" {
		t.Fatalf("after a restart:\n%s", rowsOf(card))
	}
	if r2.ask("nn").Rows[0].Title != "Download the Top 4" {
		t.Fatal("How Many was forgotten")
	}
}

func TestAlbumDetails(t *testing.T) {
	f := newFake(t)
	f.cutOnce[501] = true
	r := newRig(t, f, "")
	page := f.URL + "/album/loves-arp"
	r.ask("aotd") // the pages' releases are known by their rows
	if rep := r.ask("get", page); rep.Message != "Downloading “Love's Arp”" {
		t.Fatalf("get: %+v", rep)
	}
	if rep := r.ask("get", page); !strings.HasPrefix(rep.Message, "Downloading") && rep.Message != "Waiting to download" {
		t.Fatalf("get again: %+v", rep)
	}
	r.idle()
	dir := filepath.Join(r.card, "Bandcamp", "Dawuna", "2026 - Love's Arp")
	tg, au := tags(t, filepath.Join(dir, "01 - Love.mp3"))
	if !bytes.Equal(au, audio(501)) || tg["TRCK"] != "1/3" || tg["TPUB"] != "True Panther" {
		t.Fatalf("the resumed song: tags %q, %d bytes", tg, len(au))
	}
	if f.count("/stream/501.mp3") != 2 {
		t.Fatalf("501 asked %d times", f.count("/stream/501.mp3"))
	}
	tg, _ = tags(t, filepath.Join(dir, "02 - Love's Ekongo.mp3"))
	if tg["USLT"] != "\x03eng\x00first line\nsecond line" || tg["TCON"] != "Electronic" {
		t.Fatalf("lyrics: %q", tg)
	}
	if _, err := os.Stat(filepath.Join(dir, "03 - Unreleased.mp3")); err == nil {
		t.Fatal("a track Bandcamp doesn't stream was downloaded")
	}
	st, j := r.d.dl.state(releaseKey(page))
	if st != "done" || j.Done != 2 || j.Missing != 1 || j.Total != 3 {
		t.Fatalf("its job: %s %+v", st, j)
	}
	if rep := r.ask("get", page); rep.Message != "It's on the card already" {
		t.Fatalf("get when done: %+v", rep)
	}
	// a song deleted: Download Again (retry) fetches it alone
	os.Remove(filepath.Join(dir, "02 - Love's Ekongo.mp3"))
	before := f.count("/stream/501.mp3")
	if rep := r.ask("retry", releaseKey(page)); rep.Message != "Trying again" {
		t.Fatalf("retry: %+v", rep)
	}
	r.idle()
	if _, err := os.Stat(filepath.Join(dir, "02 - Love's Ekongo.mp3")); err != nil || f.count("/stream/501.mp3") != before {
		t.Fatalf("again: %v, 501 asked %d times", err, f.count("/stream/501.mp3"))
	}
	// a release that's gone fails, and says so
	if rep := r.ask("get", f.URL+"/album/nope"); !strings.HasPrefix(rep.Message, "Downloading") {
		t.Fatalf("get gone: %+v", rep)
	}
	r.idle()
	if st, j := r.d.dl.state(releaseKey(f.URL + "/album/nope")); st != "fail" || j.Err != errGone.Error() {
		t.Fatalf("gone: %s %+v", st, j)
	}
	if !strings.Contains(rowsOf(r.ask("downloads")), "Couldn't download: Not on Bandcamp any more · press to try again") {
		t.Fatalf("downloads:\n%s", rowsOf(r.ask("downloads")))
	}
}

func TestPlaylistAndShowAsAlbums(t *testing.T) {
	f := newFake(t)
	r := newRig(t, f, "")
	pls := r.ask("playlists")
	if pls.Rows[0].Kind != "list" || pls.Rows[0].URI != "playlist" || pls.Rows[0].Context != "77" ||
		pls.Rows[0].Subtitle != "David James · 4 songs" {
		t.Fatalf("playlists:\n%s", rowsOf(pls))
	}
	pl := r.ask("playlist", "77")
	got := rowsOf(pl)
	if pl.Rows[0].URI != "getlist" || pl.Rows[0].Title != "Download Playlist" || pl.Rows[1].Kind != "info" ||
		pl.Rows[2].URI != "getalbum" || pl.Rows[2].Context != "playlist:77:0" || pl.Rows[2].Position != "" ||
		!strings.HasPrefix(pl.Rows[4].Subtitle, "Not in this list's download") {
		t.Fatalf("playlist:\n%s", got)
	}
	if rep := r.ask("getlist", "playlist:77"); rep.Message != "Downloading “Night Drive”" {
		t.Fatalf("getlist: %+v", rep)
	}
	r.idle()
	dir := filepath.Join(r.card, "Bandcamp", "Playlists", "Night Drive")
	tg, _ := tags(t, filepath.Join(dir, "04 - One.mp3"))
	if tg["TALB"] != "Night Drive" || tg["TPE2"] != "Playlists" || tg["TCMP"] != "1" || tg["TRCK"] != "4/4" ||
		tg["TPE1"] != "Artist 3" || tg["WOAF"] != f.URL+"/album/a3" {
		t.Fatalf("a playlist song's tags: %q", tg)
	}
	for _, name := range []string{"01 - One.mp3", "02 - One.mp3", "cover.jpg", ".q2-playlist"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "03 - Unreleased.mp3")); err == nil {
		t.Fatal("a song Bandcamp doesn't stream was downloaded")
	}
	pl = r.ask("playlist", "77")
	if pl.Rows[0].Kind != "local" || pl.Rows[0].Title != "On the Card" || pl.Rows[1].Title != "Download Again" ||
		pl.Rows[2].URI != "delete" || pl.Rows[4].Kind != "local" || pl.Rows[4].URI != filepath.Join(dir, "01 - One.mp3") {
		t.Fatalf("playlist after:\n%s", rowsOf(pl))
	}

	// the radio: series, a show, its songs kept as an album
	radio := r.ask("radio")
	got = rowsOf(radio)
	if radio.Rows[0].Title != "Latest Shows" || radio.Rows[1].Title != "Bandcamp Selects" ||
		!strings.Contains(got, "|The Hip Hop Show|1 show") || !strings.Contains(got, "|Bandcamp Weekly|60 shows") {
		t.Fatalf("radio:\n%s", got)
	}
	show := r.ask("show", "1018")
	got = rowsOf(show)
	if show.Rows[0].Title != "Belfast's jazz-punks." || show.Rows[1].Title != "Download the Songs" || show.Rows[2].Kind != "info" ||
		show.Rows[4].URI != "getalbum" || show.Rows[4].Subtitle != "3:10 · Artist 5 · Album 5" {
		t.Fatalf("show:\n%s", got)
	}
	r.ask("getlist", "radio:1018")
	r.idle()
	dir = filepath.Join(r.card, "Bandcamp", "Radio", "2026-11-09 - Bandcamp Selects - Robocobra Quartet")
	tg, _ = tags(t, filepath.Join(dir, "03 - Two_ Part_2_.mp3"))
	if tg["TALB"] != "Bandcamp Selects: Robocobra Quartet" || tg["TPE2"] != "Bandcamp Radio" || tg["TCMP"] != "1" ||
		tg["TRCK"] != "3/3" || tg["TDRC"] != "2026" || tg["TPE1"] != "Artist 6" {
		t.Fatalf("a show song's tags: %q", tg)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".q2-playlist")); !strings.Contains(string(b), "radio 1018") {
		t.Fatalf(".q2-playlist: %q", b)
	}
	// a press on a show's song downloads the release it is on
	if rep := r.ask("getalbum", "radio:1018:0"); rep.Message != "Downloading “Album 4”" {
		t.Fatalf("getalbum: %+v", rep)
	}
	r.idle()
	if _, err := os.Stat(filepath.Join(r.card, "Bandcamp", "Artist 4", "2026 - Album 4", "01 - One.mp3")); err != nil {
		t.Fatal(err)
	}
}

func TestSearchArtistFan(t *testing.T) {
	f := newFake(t)
	r := newRig(t, f, "")
	rep := r.ask("search", "dawuna")
	got := rowsOf(rep)
	if len(rep.Rows) != 3 || rep.Rows[0].URI != "get" || rep.Rows[0].Subtitle != "Album · Dawuna" ||
		rep.Rows[1].URI != "artist" || rep.Rows[1].Context != f.URL+"/band" || rep.Rows[2].URI != "fan" || rep.Rows[2].Context != "613839" {
		t.Fatalf("search:\n%s", got)
	}
	art := r.ask("artist", f.URL+"/band")
	got = rowsOf(art)
	if len(art.Rows) != 3 || art.Rows[0].URI != "getall" || art.Rows[1].Title != "Album 1" || art.Rows[1].Subtitle != "Guest" ||
		art.Rows[2].Title != "Album 2" || art.Rows[2].Subtitle != "Dawuna" {
		t.Fatalf("artist:\n%s", got)
	}
	if rep := r.ask("getall", f.URL+"/band"); rep.Message != "Downloading 2 releases" {
		t.Fatalf("getall: %+v", rep)
	}
	r.idle()
	fan := r.ask("fan", "613839")
	if fan.Rows[0].Context != "77" {
		t.Fatalf("fan:\n%s", rowsOf(fan))
	}
	if fa, err := r.d.bc.FanByName(context.Background(), "funkentechno"); err != nil || fa.ID != 613839 {
		t.Fatalf("fan by name: %+v %v", fa, err)
	}
}

func TestWindows(t *testing.T) {
	f := newFake(t)
	r := newRig(t, f, "")
	all := r.ask("series", "")
	if len(all.Rows) != perPage+1 || all.Rows[perPage].Kind != "page" || all.Rows[perPage].URI != "next" ||
		all.Rows[perPage].Context != "48" || all.Rows[perPage].Subtitle != "49–63 of 63" {
		t.Fatalf("first page: %d rows, last %+v", len(all.Rows), all.Rows[len(all.Rows)-1])
	}
	next := r.ask("series", "", "48")
	if next.Rows[0].Kind != "page" || next.Rows[0].URI != "prev" || next.Rows[0].Context != "" ||
		len(next.Rows) != 16 || next.Rows[len(next.Rows)-1].Kind == "page" {
		t.Fatalf("second page:\n%s", rowsOf(next))
	}
	// the head row takes room on the first page only
	nn := r.d.window([]item{{row: info("head", "")}}, make([]item, 100), "", "")
	if len(nn.Rows) != 49 || nn.Rows[48].Context != "47" {
		t.Fatalf("with a head: %d rows, last %+v", len(nn.Rows), nn.Rows[48])
	}
	p2 := r.d.window([]item{{row: info("head", "")}}, make([]item, 100), "47", "")
	if p2.Rows[0].Context != "" || p2.Rows[len(p2.Rows)-1].Context != "95" || p2.Rows[0].Subtitle != "1–47 of 100" {
		t.Fatalf("page 2: first %+v, last %+v", p2.Rows[0], p2.Rows[len(p2.Rows)-1])
	}
	p3 := r.d.window([]item{{row: info("head", "")}}, make([]item, 100), "95", "")
	if p3.Rows[0].Context != "47" || len(p3.Rows) != 6 {
		t.Fatalf("page 3: %d rows, first %+v", len(p3.Rows), p3.Rows[0])
	}
	// without "window": a cursor and More…
	os.WriteFile(r.caps, []byte("progress art\n"), 0o644)
	old := r.ask("series", "")
	if old.Cursor != "48" || len(old.Rows) != perPage {
		t.Fatalf("old firmware: cursor %q, %d rows", old.Cursor, len(old.Rows))
	}
	if enc, kept := old.Encode("1"); kept != perPage || len(enc) > q2spot.MaxReply {
		t.Fatalf("encoded %d of %d rows", kept, perPage)
	}
}

func TestRestartResumes(t *testing.T) {
	f := newFake(t)
	f.hold = make(chan struct{})
	base := t.TempDir()
	r := newRig(t, f, base)
	r.ask("get", f.URL+"/album/a5")
	wait(t, "the download to start", func() bool { return r.d.dl.snap().cur })
	r.stop()
	q, _ := os.ReadFile(filepath.Join(r.dir, "queue.json"))
	if !strings.Contains(string(q), "album/a5") {
		t.Fatalf("queue.json: %s", q)
	}
	close(f.hold)
	f.mu.Lock()
	f.hold = nil
	f.mu.Unlock()
	r2 := newRig(t, f, base)
	r2.idle()
	if _, err := os.Stat(filepath.Join(r2.card, "Bandcamp", "Artist 5", "2026 - Album 5", "02 - Two_ Part_2_.mp3")); err != nil {
		t.Fatal(err)
	}
	// the stop op answers, then stops it
	if rep := r2.ask("stop"); rep.Message != "Bandcamp stopped" {
		t.Fatalf("stop: %+v", rep)
	}
	select {
	case <-r2.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop didn't stop it")
	}
}

func TestSocket(t *testing.T) {
	f := newFake(t)
	r := newRig(t, f, "")
	wait(t, "the socket", func() bool { _, err := os.Stat(r.d.browse); return err == nil })
	c, err := net.Dial("unix", r.d.browse)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("Q2SPOT/1\n7\tmenu\n"))
	c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	c.Close()
	if !strings.HasPrefix(string(b), "Q2SPOT/1\n7\tok\t") || !strings.Contains(string(b), "list\tnn\tNew and Notable") {
		t.Fatalf("reply: %q", b)
	}
	if st, _ := os.ReadFile(r.tmp + ".state"); !strings.Contains(string(st), "version="+version) {
		t.Fatalf("state: %q", st)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"AC/DC: Live?":   "AC_DC_ Live_",
		" ...  ":         "Unknown Artist",
		"tab\there\x00x": "tabherex",
	} {
		if got := safe(in, artistMax, "Unknown Artist"); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
	if got := songFile(7, 120, "Song"); got != "007 - Song.mp3" {
		t.Errorf("songFile: %q", got)
	}
	long := strings.Repeat("é", 200)
	p := filepath.Join("/mnt/mmc/Bandcamp", albumDir(long, long, time.Now()), songFile(1, 10, long))
	if len(p) >= 240 {
		t.Errorf("a path of %d bytes", len(p))
	}
	if got := showDir("Bandcamp Weekly", "Bandcamp Weekly", time.Date(2013, 4, 9, 0, 0, 0, 0, time.UTC)); got != "Radio/2013-04-09 - Bandcamp Weekly" {
		t.Errorf("showDir: %q", got)
	}
	if releaseKey("https://Dawuna.bandcamp.com/album/loves-arp/?from=hp") != "album:dawuna.bandcamp.com/album/loves-arp" {
		t.Error(releaseKey("https://Dawuna.bandcamp.com/album/loves-arp/?from=hp"))
	}
	if bandcamp.Series("The Hip-Hop Show ") != "The Hip Hop Show" || bandcamp.Series("Bandcamp Weekly № 12") != "Bandcamp Weekly" {
		t.Error("Series")
	}
}
