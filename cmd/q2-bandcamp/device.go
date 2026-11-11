package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
)

// The Q2's Bandcamp page (Q2 Pod's card app page, q2-pod's patch/addon.c), over Q2SPOT/1. Each row's
// kind says what a press does: list opens a level (its uri the op, its context the argument),
// action sends its uri with its context and toasts the reply, search opens the keyboard (op
// search), local plays from the card (a folder, "/" last, lists its songs; context "album": in
// track order), page moves a long list to its previous or next page (Q2 Pod's "window"; older
// firmware gets a cursor and More…), progress draws a bar. A row's position is the op a held
// Play/Pause sends, with its context.
//
//	menu                       New and Notable, Album of the Day, Playlists, Bandcamp Radio, Search,
//	                           Downloads, On the Card, Stop Bandcamp
//	nn, aotd [cursor]          Download the Top 10 / the Latest 10 (top10 nn|aotd), then the releases
//	get <page>                 a release downloaded (an album's row; again on one that failed)
//	play <page>                a release played (Play/Pause held on its row)
//	playlists, fan <id>        featured playlists, a fan's: rows open playlist <id>
//	playlist <id> [cursor]     Play Playlist, Download Playlist (getlist), its songs
//	radio                      Latest Shows and each series (series <name>), then show <id>
//	show <id> [cursor]         Play the Show (playshow: the broadcast), Play the Songs, Download the
//	                           Songs (getlist), the songs
//	playfrom <kind>:<id>:<n>   a playlist's or show's songs played from the n'th
//	getalbum <kind>:<id>:<n>   the release a playlist's or show's song is on, downloaded (held)
//	search <text>              releases, artists and labels (artist <url>), fans (fan <id>)
//	artist <url> [cursor]      Download All (getall), then its releases
//	downloads [cursor]         the queue, finished jobs, failures (retry), cancel, cancelall, forget
//	card [cursor]              everything downloaded, the newest first
//	stop                       the app stops
//
// A list whose rows show a download under way says "refresh" and is asked again every 2 s while on
// top. Lists come from Bandcamp through the cache (cache.go).

const perPage = 48 // a page's rows, past a Previous and a Next row (Q2SPOT/1 sends 50 at most)

type item struct {
	row q2spot.Row
	pic bandcamp.Pic
}

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func okMsg(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }

// fail is an error as the page shows it.
func fail(err error) q2spot.Reply {
	switch {
	case errors.Is(err, errLoading):
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Loading…", "Bandcamp is sending a long list")}}
	case errors.Is(err, errNoURLs):
		return q2spot.Fail(q2spot.Unsupported, err.Error())
	case errors.Is(err, bandcamp.ErrNotFound):
		return q2spot.Fail(q2spot.Unavailable, "It isn't on Bandcamp any more")
	case errors.Is(err, bandcamp.ErrBlocked):
		return q2spot.Fail(q2spot.Refused, "Bandcamp refused the Q2 for now; try again later")
	case bandcamp.IsNoNetwork(err):
		return q2spot.Fail(q2spot.Offline, "Bandcamp can't be reached")
	}
	return q2spot.Fail(q2spot.Error, "Bandcamp didn't answer")
}

// device answers the Q2's page; a slow answer (the Q2 waits 15 s) is logged.
func (d *daemon) device(req q2spot.Request) q2spot.Reply {
	t := time.Now()
	r := d.answer(req)
	if el := time.Since(t); el > 3*time.Second || r.Status != "" && r.Status != q2spot.OK {
		logf("page: %s %q took %.1f s (%s %s)", req.Op, req.Args, el.Seconds(), r.Status, r.Message)
	}
	return r
}

var ops = []string{"caps", "menu", "nn", "aotd", "top10", "get", "getall", "play", "playlists", "fan", "playlist",
	"radio", "series", "show", "playfrom", "playshow", "getlist", "getalbum", "search", "artist", "downloads",
	"cancel", "cancelall", "retry", "forget", "card", "stop"}

func (d *daemon) answer(req q2spot.Request) q2spot.Reply {
	d.touch()
	a0, a1 := req.Arg(0), req.Arg(1)
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range ops {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return d.menu()
	case "nn", "aotd":
		return d.picks(req.Op, a0)
	case "top10":
		return d.top10(a0)
	case "get":
		return d.getRelease(a0)
	case "getall":
		return d.getAll(a0)
	case "play":
		return d.playRelease(a0)
	case "playlists":
		return d.featured(a0)
	case "fan":
		return d.fan(a0, a1)
	case "playlist":
		return d.listPage("playlist", a0, a1)
	case "show":
		return d.listPage("radio", a0, a1)
	case "radio":
		return d.radio()
	case "series":
		return d.series(a0, a1)
	case "playfrom":
		return d.playFrom(a0)
	case "playshow":
		return d.playShow(a0)
	case "getlist":
		return d.getList(a0)
	case "getalbum":
		return d.getSongAlbum(a0)
	case "search":
		return d.search(a0)
	case "artist":
		return d.artist(a0, a1)
	case "downloads":
		return d.downloadsPage(a0)
	case "card":
		return d.cardPage(a0)
	case "cancel":
		if !d.dl.remove(a0) {
			return q2spot.Fail(q2spot.Unavailable, "It isn't waiting any more")
		}
		return okMsg("Removed")
	case "cancelall":
		return okMsg(plural(d.dl.clear(), "download") + " removed")
	case "retry":
		return d.retry(a0)
	case "forget":
		d.dl.forget()
		return okMsg("Cleared")
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); d.shutdown() }() // the reply first
		return okMsg("Bandcamp stopped")
	}
	return q2spot.Fail(q2spot.Unsupported, "Q2 Pod asked for something this Bandcamp doesn't know")
}

// window is one page of a long list: head rows first on the first, then items, perPage rows a page,
// with Previous and Next rows (Q2 Pod's "window"), or a cursor and More… on firmware without it.
// The rows' covers are put in place before it answers; msg is the reply's.
func (d *daemon) window(head, items []item, cursor, msg string) q2spot.Reply {
	first := perPage - len(head) // the first page's items
	from, _ := strconv.Atoi(cursor)
	if from < 0 || from >= len(items) {
		from = 0
	}
	if from > 0 && from < first {
		from = first
	}
	end := first
	if from > 0 {
		end = from + perPage
	}
	end = min(end, len(items))
	var shown []item
	paged := q2.HasCap("window")
	if from == 0 {
		shown = append(shown, head...)
	} else if paged {
		prev := from - perPage // pages past the first start at first, first+perPage, …
		c, start := strconv.Itoa(prev), prev
		if prev < first {
			c, start = "", 0
		}
		shown = append(shown, item{row: q2spot.Row{Kind: "page", URI: "prev", Context: c, Title: "Previous Page",
			Subtitle: fmt.Sprintf("%d–%d of %d", start+1, from, len(items))}})
	}
	shown = append(shown, items[from:end]...)
	r := q2spot.Reply{Message: msg}
	if end < len(items) {
		if paged {
			shown = append(shown, item{row: q2spot.Row{Kind: "page", URI: "next", Context: strconv.Itoa(end),
				Title: "Next Page", Subtitle: fmt.Sprintf("%d–%d of %d", end+1, min(end+perPage, len(items)), len(items))}})
		} else {
			r.Cursor = strconv.Itoa(end)
		}
	}
	rows := make([]q2spot.Row, len(shown))
	pics := make([]bandcamp.Pic, len(shown))
	for i, it := range shown {
		rows[i], pics[i] = it.row, it.pic
	}
	d.art.ensure(rows, pics)
	r.Rows = rows
	return r
}

func liveMsg(live bool) string {
	if live {
		return "refresh"
	}
	return ""
}

// The menu.
func (d *daemon) menu() q2spot.Reply {
	rows := []q2spot.Row{
		{Kind: "list", URI: "nn", Title: "New and Notable", Subtitle: "Picked by Bandcamp's editors"},
		{Kind: "list", URI: "aotd", Title: "Album of the Day", Subtitle: "Bandcamp Daily's reviews"},
		{Kind: "list", URI: "playlists", Title: "Playlists", Subtitle: "Featured; search a fan's name for theirs"},
		{Kind: "list", URI: "radio", Title: "Bandcamp Radio", Subtitle: "Every show, by series"},
		{Kind: "search", Title: "Search Bandcamp", Subtitle: "Albums, artists, labels and fans"},
	}
	s := d.dl.snap()
	if len(s.queue) > 0 || len(s.recent) > 0 {
		rows = append(rows, q2spot.Row{Kind: "list", URI: "downloads", Title: "Downloads", Subtitle: queueLine(s)})
	}
	if d.dl.anyOnCard() {
		rows = append(rows, q2spot.Row{Kind: "list", URI: "card", Title: "On the Card", Subtitle: "What you downloaded, in Bandcamp/"})
	}
	rows = append(rows, q2spot.Row{Kind: "action", URI: "stop", Title: "Stop Bandcamp", Subtitle: "Downloads go on at its next start"})
	return q2spot.Reply{Message: liveMsg(len(s.queue) > 0), Rows: rows}
}

// queueLine is the Downloads row's subtitle: what is under way.
func queueLine(s snapshot) string {
	waiting := len(s.queue)
	switch {
	case s.stalled != "" && waiting > 0:
		return "Waiting: " + s.stalled
	case s.cur:
		j := s.queue[0]
		line := j.Title
		if j.Total > 0 {
			line += fmt.Sprintf(" · song %d of %d", s.songN, j.Total)
		}
		if waiting > 1 {
			line += " · " + strconv.Itoa(waiting-1) + " more"
		}
		return line
	case waiting > 0:
		return plural(waiting, "download") + " waiting"
	}
	bad := 0
	for _, j := range s.recent {
		if j.Err != "" || len(j.Failed) > 0 {
			bad++
		}
	}
	if bad > 0 {
		return plural(bad, "download") + " with failures"
	}
	return plural(len(s.recent), "download") + " finished"
}

// status is a job's state as a row's subtitle says it, and whether it is under way or waiting.
func (d *daemon) status(key string) (string, bool) {
	st, j := d.dl.state(key)
	switch st {
	case "run":
		s := d.dl.snap()
		if s.stalled != "" {
			return "Waiting: " + s.stalled, true
		}
		if j.Total == 0 {
			return "Downloading…", true
		}
		return fmt.Sprintf("Downloading %d of %d", s.songN, j.Total), true
	case "wait":
		return "Waiting to download", true
	case "fail":
		return "Couldn't download: " + j.Err, false
	case "some":
		return fmt.Sprintf("%d of %d on the card", j.Done, j.Total), false
	case "done":
		return "On the card", false
	}
	return "", false
}

// albumItem is a release's row: on the card, a local row (a press plays it); else a press
// downloads it and Play/Pause held plays it.
func (d *daemon) albumItem(a bandcamp.Album, sub string) (item, bool) {
	key := releaseKey(a.URL)
	pic := bandcamp.Pic{ID: a.Art, Art: true}
	if dir := d.dl.folder(key); dir != "" {
		st, _ := d.status(key)
		return item{q2spot.Row{Kind: "local", URI: dir + "/", Context: "album", Title: a.Title,
			Subtitle: joinNonEmpty(" · ", st, a.Artist)}, pic}, false
	}
	st, live := d.status(key)
	if st != "" {
		sub = joinNonEmpty(" · ", st, a.Artist)
		if strings.HasPrefix(st, "Couldn't") {
			sub += " · press to try again"
		}
	}
	return item{q2spot.Row{Kind: "action", URI: "get", Context: a.URL, Title: a.Title, Subtitle: sub, Position: "play"}, pic}, live
}

func (d *daemon) albumItems(as []bandcamp.Album, sub func(bandcamp.Album) string) ([]item, bool) {
	d.remember(as)
	items := make([]item, 0, len(as))
	live := false
	for _, a := range as {
		it, l := d.albumItem(a, sub(a))
		items = append(items, it)
		live = live || l
	}
	return items, live
}

// picksList is New and Notable's or Album of the Day's releases, 120 of them.
func (d *daemon) picksList(which string, fresh bool) ([]bandcamp.Album, error) {
	v, err := d.cache.get("picks:"+which, 30*time.Minute, fresh, func(ctx context.Context) (any, error) {
		if which == "aotd" {
			return d.bc.AlbumsOfTheDay(ctx, 120)
		}
		return d.bc.NewAndNotable(ctx, 120)
	})
	if err != nil {
		return nil, err
	}
	return v.([]bandcamp.Album), nil
}

func (d *daemon) picks(which, cursor string) q2spot.Reply {
	as, err := d.picksList(which, false)
	if err != nil {
		return fail(err)
	}
	sub := func(a bandcamp.Album) string { return joinNonEmpty(" · ", a.Artist, a.Genre) }
	if which == "aotd" {
		sub = func(a bandcamp.Album) string { return joinNonEmpty(" · ", a.Artist, day(a.Date)) }
	}
	items, live := d.albumItems(as, sub)
	head, hlive := d.top10Row(which, as)
	return d.window([]item{head}, items, cursor, liveMsg(live || hlive))
}

func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Year() != time.Now().Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// top10Row is Download the Top 10 (New and Notable) or the Latest 10 (Album of the Day), saying how
// many of those are on the card or coming.
func (d *daemon) top10Row(which string, as []bandcamp.Album) (item, bool) {
	title, sub := "Download the Top 10", "The editors' 10 newest picks"
	if which == "aotd" {
		title, sub = "Download the Latest 10", "The 10 latest Albums of the Day"
	}
	have, coming := 0, 0
	for _, a := range as[:min(10, len(as))] {
		switch st, _ := d.dl.state(releaseKey(a.URL)); st {
		case "done", "some":
			have++
		case "run", "wait":
			coming++
		}
	}
	switch {
	case have == min(10, len(as)) && have > 0:
		sub = "All of them are on the card"
	case coming > 0:
		sub = fmt.Sprintf("%d on the card · %d downloading", have, coming)
	case have > 0:
		sub = fmt.Sprintf("%d of them on the card · press for the rest", have)
	}
	return item{row: q2spot.Row{Kind: "action", URI: "top10", Context: which, Title: title, Subtitle: sub}}, coming > 0
}

// top10 queues the list's first 10 releases as they are now, those not on the card.
func (d *daemon) top10(which string) q2spot.Reply {
	if which != "nn" && which != "aotd" {
		return q2spot.Fail(q2spot.Bad, "Which list?")
	}
	as, err := d.picksList(which, true)
	if err != nil {
		return fail(err)
	}
	d.remember(as)
	added, had, queued := 0, 0, 0
	for _, a := range as[:min(10, len(as))] {
		switch d.dl.add(albumJob(a), false) {
		case "added":
			added++
		case "done":
			had++
		default:
			queued++
		}
	}
	switch {
	case added == 0 && had > 0 && queued == 0:
		return okMsg("All 10 are on the card already")
	case added == 0:
		return okMsg("They're in Downloads already")
	case had > 0:
		return okMsg(fmt.Sprintf("Downloading %s; %d on the card already", plural(added, "album"), had))
	}
	return okMsg("Downloading " + plural(added, "album"))
}

func albumJob(a bandcamp.Album) *job {
	title := a.Title
	if title == "" {
		title = strings.ReplaceAll(filepath.Base(a.URL), "-", " ")
	}
	return &job{Key: releaseKey(a.URL), Kind: "album", URL: a.URL, Title: title, Artist: a.Artist, Genre: a.Genre,
		Pic: bandcamp.Pic{ID: a.Art, Art: true}.Key()}
}

// getRelease downloads a release (a press on its row), or tries a failed one again.
func (d *daemon) getRelease(page string) q2spot.Reply {
	if !strings.HasPrefix(page, "http") {
		return q2spot.Fail(q2spot.Bad, "Which release?")
	}
	a, ok := d.lookup(page)
	if !ok {
		a = bandcamp.Album{URL: page}
	}
	st, _ := d.dl.state(releaseKey(page))
	switch d.dl.add(albumJob(a), st == "fail" || st == "some") {
	case "queued":
		s, _ := d.status(releaseKey(page))
		return okMsg(s)
	case "done":
		return okMsg("It's on the card already")
	}
	if a.Title == "" {
		return okMsg("Downloading")
	}
	return okMsg("Downloading “" + a.Title + "”")
}

// release is a release's page as Bandcamp has it, for playing it.
func (d *daemon) release(page string) (*bandcamp.Release, error) {
	v, err := d.cache.get("release:"+releaseKey(page), 10*time.Minute, false, func(ctx context.Context) (any, error) {
		return d.bc.Release(ctx, page)
	})
	if err != nil {
		return nil, err
	}
	return v.(*bandcamp.Release), nil
}

// playRelease plays a release (Play/Pause held on its row): from the card where it is there.
func (d *daemon) playRelease(page string) q2spot.Reply {
	if !q2.HasCap("urls") {
		return fail(errNoURLs)
	}
	r, err := d.release(page)
	if err != nil {
		return fail(err)
	}
	if err := d.st.start(d.st.releaseEntries(r, d.dl.folder(releaseKey(page))), 0); err != nil {
		return fail(err)
	}
	return okMsg("Playing “" + r.Title + "”")
}

// cachedList is a playlist's, a show's or a release's songs from Bandcamp's player.
func (d *daemon) cachedList(ctx context.Context, kind string, id int64) (*bandcamp.Tracklist, error) {
	v, err := d.cache.get("list:"+listKey(kind, id), 10*time.Minute, false, func(ctx context.Context) (any, error) {
		return d.bc.Tracklist(ctx, kind, id)
	})
	if err != nil {
		return nil, err
	}
	return v.(*bandcamp.Tracklist), nil
}

// The playlists.
func (d *daemon) playlistItems(pls []bandcamp.Playlist) ([]item, bool) {
	var items []item
	live := false
	for _, p := range pls {
		st, l := d.status(listKey("playlist", p.ID))
		live = live || l
		items = append(items, item{q2spot.Row{Kind: "list", URI: "playlist", Context: strconv.FormatInt(p.ID, 10), Title: p.Title,
			Subtitle: joinNonEmpty(" · ", st, p.Curator, plural(p.Count, "song"))}, bandcamp.Pic{ID: p.Image}})
	}
	return items, live
}

func (d *daemon) featured(cursor string) q2spot.Reply {
	v, err := d.cache.get("featured", 30*time.Minute, false, func(ctx context.Context) (any, error) {
		return d.bc.FeaturedPlaylists(ctx)
	})
	if err != nil {
		return fail(err)
	}
	items, live := d.playlistItems(v.([]bandcamp.Playlist))
	items = append(items, item{row: info("A fan's playlists", "Search Bandcamp for the fan's name")})
	return d.window(nil, items, cursor, liveMsg(live))
}

func (d *daemon) fan(arg, cursor string) q2spot.Reply {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return q2spot.Fail(q2spot.Bad, "Which fan?")
	}
	v, err := d.cache.get("fan:"+arg, 10*time.Minute, false, func(ctx context.Context) (any, error) {
		return d.bc.FanPlaylists(ctx, id)
	})
	if err != nil {
		return fail(err)
	}
	items, live := d.playlistItems(v.([]bandcamp.Playlist))
	if len(items) == 0 {
		items = append(items, item{row: info("No public playlists", "This fan hasn't shared any")})
	}
	return d.window(nil, items, cursor, liveMsg(live))
}

// parseList reads "<kind>:<id>" and "<kind>:<id>:<n>".
func parseList(s string) (kind string, id int64, n int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || (parts[0] != "playlist" && parts[0] != "radio") {
		return "", 0, 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	if len(parts) > 2 {
		if n, err = strconv.Atoi(parts[2]); err != nil || n < 0 {
			return "", 0, 0, false
		}
	}
	return parts[0], id, n, true
}

// listPage is a playlist's or a radio show's page: playing it, downloading it, its songs.
func (d *daemon) listPage(kind, arg, cursor string) q2spot.Reply {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return q2spot.Fail(q2spot.Bad, "Which list?")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tl, err := d.cachedList(ctx, kind, id)
	if err != nil {
		return fail(err)
	}
	ref := listKey(kind, id)
	streams := 0
	for _, t := range tl.Tracks {
		if t.Stream != "" {
			streams++
		}
	}
	var head []item
	if kind == "radio" {
		if s, ok := d.showInfo(id); ok && s.Blurb != "" {
			head = append(head, item{row: info(s.Blurb, day(s.Date))})
		}
		head = append(head, item{row: q2spot.Row{Kind: "action", URI: "playshow", Context: arg, Title: "Play the Show",
			Subtitle: "The broadcast as it aired · " + clock(tl.Seconds)}, pic: tl.Image})
		head = append(head, item{row: q2spot.Row{Kind: "action", URI: "playfrom", Context: ref + ":0", Title: "Play the Songs",
			Subtitle: plural(streams, "song") + ", without the talk"}})
	} else {
		secs := 0.0
		for _, t := range tl.Tracks {
			secs += t.Seconds
		}
		head = append(head, item{row: q2spot.Row{Kind: "action", URI: "playfrom", Context: ref + ":0", Title: "Play Playlist",
			Subtitle: joinNonEmpty(" · ", plural(streams, "song"), clock(secs), tl.Artist)}, pic: tl.Image})
	}
	dl, live := d.listDownloadRow(kind, ref, tl)
	head = append(head, dl...)
	dir := d.dl.folder(ref)
	items := make([]item, 0, len(tl.Tracks))
	for i, t := range tl.Tracks {
		sub := joinNonEmpty(" · ", t.Artist, t.Album)
		if kind == "radio" && t.At >= 0 {
			sub = joinNonEmpty(" · ", clock(t.At), sub)
		}
		pic := bandcamp.Pic{ID: t.Art, Art: true}
		if t.Stream == "" {
			items = append(items, item{info(t.Title, joinNonEmpty(" · ", "Bandcamp doesn't stream it", t.Artist)), pic})
			continue
		}
		ctxArg := ref + ":" + strconv.Itoa(i)
		if dir != "" {
			if p := filepath.Join(dir, songFile(i+1, len(tl.Tracks), t.Title)); exists(p) {
				items = append(items, item{q2spot.Row{Kind: "local", URI: p, Context: "album", Title: t.Title, Subtitle: sub}, pic})
				continue
			}
		}
		items = append(items, item{q2spot.Row{Kind: "action", URI: "playfrom", Context: ctxArg, Title: t.Title, Subtitle: sub,
			Position: "getalbum"}, pic})
	}
	return d.window(head, items, cursor, liveMsg(live))
}

// listDownloadRow is a list's download row(s): on the card, its folder; else Download, with its
// state.
func (d *daemon) listDownloadRow(kind, ref string, tl *bandcamp.Tracklist) ([]item, bool) {
	title, where := "Download Playlist", "As an album, in Bandcamp/Playlists/"
	if kind == "radio" {
		title, where = "Download the Songs", "As an album, in Bandcamp/Radio/"
	}
	st, live := d.status(ref)
	if dir := d.dl.folder(ref); dir != "" {
		return []item{
			{row: q2spot.Row{Kind: "local", URI: dir + "/", Context: "album", Title: "On the Card", Subtitle: joinNonEmpty(" · ", st,
				strings.TrimPrefix(dir, filepath.Dir(d.root)+"/"))}, pic: tl.Image},
			{row: q2spot.Row{Kind: "action", URI: "getlist", Context: ref, Title: "Download Again",
				Subtitle: "Fetches the songs that aren't on the card"}},
		}, live
	}
	sub := where
	if st != "" {
		sub = st
		if strings.HasPrefix(st, "Couldn't") {
			sub += " · press to try again"
		}
	}
	return []item{{row: q2spot.Row{Kind: "action", URI: "getlist", Context: ref, Title: title, Subtitle: sub}}}, live
}

// showInfo is a show as Bandcamp Radio's list has it, if that was fetched (never waited for).
func (d *daemon) showInfo(id int64) (bandcamp.Show, bool) {
	v, ok := d.cache.peek("radio")
	if !ok {
		return bandcamp.Show{}, false
	}
	for _, s := range v.([]bandcamp.Show) {
		if s.ID == id {
			return s, true
		}
	}
	return bandcamp.Show{}, false
}

// playFrom plays a playlist's or a show's songs from the n'th.
func (d *daemon) playFrom(arg string) q2spot.Reply {
	kind, id, n, ok := parseList(arg)
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Which list?")
	}
	if !q2.HasCap("urls") {
		return fail(errNoURLs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tl, err := d.cachedList(ctx, kind, id)
	if err != nil {
		return fail(err)
	}
	es, at := d.st.listEntries(tl, d.dl.folder(listKey(kind, id)))
	from := 0
	for i := n; i < len(at); i++ {
		if at[i] >= 0 {
			from = at[i]
			break
		}
	}
	if err := d.st.start(es, from); err != nil {
		return fail(err)
	}
	return okMsg("Playing “" + es[from].title + "”")
}

// playShow plays a radio show's broadcast, talk and all.
func (d *daemon) playShow(arg string) q2spot.Reply {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return q2spot.Fail(q2spot.Bad, "Which show?")
	}
	if !q2.HasCap("urls") {
		return fail(errNoURLs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tl, err := d.cachedList(ctx, "radio", id)
	if err != nil {
		return fail(err)
	}
	if tl.Stream == "" {
		return q2spot.Fail(q2spot.Unavailable, "Bandcamp has no broadcast of this show")
	}
	series := cmpOr(bandcamp.Series(tl.Subtitle), "Bandcamp Radio")
	e := entry{url: d.st.showURL(id), title: joinNonEmpty(" · ", tl.Title, day(tl.Date)), artist: series,
		album: "Bandcamp Radio", pic: tl.Image, secs: tl.Seconds}
	if err := d.st.start([]entry{e}, 0); err != nil {
		return fail(err)
	}
	return okMsg("Playing “" + tl.Title + "”")
}

// getList downloads a playlist or a show's songs, as an album; again, the songs not on the card.
func (d *daemon) getList(arg string) q2spot.Reply {
	kind, id, _, ok := parseList(arg)
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Which list?")
	}
	j := &job{Key: listKey(kind, id), Kind: kind, ID: id, Title: "Playlist"}
	if kind == "radio" {
		j.Title = "Radio show"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if tl, err := d.cachedList(ctx, kind, id); err == nil {
		j.Title, j.Artist, j.Pic = tl.Title, tl.Artist, tl.Image.Key()
		if kind == "radio" {
			j.Artist = bandcamp.Series(tl.Subtitle)
			j.Title = joinNonEmpty(": ", j.Artist, tl.Title)
		}
	}
	if d.dl.add(j, true) == "queued" {
		s, _ := d.status(j.Key)
		return okMsg(s)
	}
	return okMsg("Downloading “" + j.Title + "”")
}

// getSongAlbum downloads the release a playlist's or a show's song is on (Play/Pause held on it).
func (d *daemon) getSongAlbum(arg string) q2spot.Reply {
	kind, id, n, ok := parseList(arg)
	if !ok {
		return q2spot.Fail(q2spot.Bad, "Which song?")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	tl, err := d.cachedList(ctx, kind, id)
	if err != nil {
		return fail(err)
	}
	if n >= len(tl.Tracks) {
		return q2spot.Fail(q2spot.Bad, "Which song?")
	}
	t := tl.Tracks[n]
	page := t.AlbumURL
	if page == "" && t.URL != "" { // a track page: its album, if it is on one
		if r, err := d.bc.Release(ctx, t.URL); err == nil && r.Album != "" {
			page = r.Album
		} else {
			page = t.URL
		}
	}
	if page == "" {
		return q2spot.Fail(q2spot.Unavailable, "Bandcamp doesn't say where it's from")
	}
	if _, ok := d.lookup(page); !ok {
		d.remember([]bandcamp.Album{{URL: page, Title: cmpOr(t.Album, t.Title), Artist: t.Artist, Art: t.Art}})
	}
	return d.getRelease(page)
}

// The radio: Latest Shows and each series, the one with the newest show first.
func (d *daemon) shows() ([]bandcamp.Show, error) {
	v, err := d.cache.get("radio", time.Hour, false, func(ctx context.Context) (any, error) { return d.bc.RadioShows(ctx) })
	if err != nil {
		return nil, err
	}
	return v.([]bandcamp.Show), nil
}

func (d *daemon) radio() q2spot.Reply {
	shows, err := d.shows()
	if err != nil {
		return fail(err)
	}
	if len(shows) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Bandcamp Radio has no shows", "")}}
	}
	type series struct {
		name   string
		n      int
		latest bandcamp.Show
	}
	by := map[string]*series{}
	var order []*series
	for _, s := range shows { // newest first
		x := by[s.Series]
		if x == nil {
			x = &series{name: s.Series, latest: s}
			by[s.Series] = x
			order = append(order, x)
		}
		x.n++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if (order[i].name == bandcamp.OtherShows) != (order[j].name == bandcamp.OtherShows) {
			return order[j].name == bandcamp.OtherShows
		}
		return false
	})
	items := []item{{q2spot.Row{Kind: "list", URI: "series", Title: "Latest Shows",
		Subtitle: "Every series, the newest first · " + day(shows[0].Date)}, bandcamp.Pic{ID: shows[0].Image}}}
	for _, x := range order {
		items = append(items, item{q2spot.Row{Kind: "list", URI: "series", Context: x.name, Title: x.name,
			Subtitle: plural(x.n, "show") + " · the latest " + day(x.latest.Date)}, bandcamp.Pic{ID: x.latest.Image}})
	}
	return d.window(nil, items, "", "")
}

// series is a series' shows, the newest first ("" every series').
func (d *daemon) series(name, cursor string) q2spot.Reply {
	shows, err := d.shows()
	if err != nil {
		return fail(err)
	}
	var items []item
	live := false
	for _, s := range shows {
		if name != "" && s.Series != name {
			continue
		}
		st, l := d.status(listKey("radio", s.ID))
		live = live || l
		sub := day(s.Date)
		if name == "" {
			sub = joinNonEmpty(" · ", sub, s.Series)
		}
		items = append(items, item{q2spot.Row{Kind: "list", URI: "show", Context: strconv.FormatInt(s.ID, 10),
			Title: cmpOr(s.Title, s.Series), Subtitle: joinNonEmpty(" · ", st, sub)}, bandcamp.Pic{ID: s.Image}})
	}
	if len(items) == 0 {
		items = append(items, item{row: info("No shows", "")})
	}
	return d.window(nil, items, cursor, liveMsg(live))
}

// Search: releases, then artists and labels, then fans.
type found struct {
	albums, artists, fans []bandcamp.Hit
}

func (d *daemon) search(text string) q2spot.Reply {
	text = strings.TrimSpace(text)
	if text == "" {
		return q2spot.Fail(q2spot.Bad, "Type something to search for")
	}
	v, err := d.cache.get("search:"+strings.ToLower(text), 5*time.Minute, false, func(ctx context.Context) (any, error) {
		var f found
		var wg sync.WaitGroup
		var errs [3]error
		for i, filter := range []string{"a", "b", "f"} {
			wg.Add(1)
			go func(i int, filter string) {
				defer wg.Done()
				hits, err := d.bc.Search(ctx, text, filter)
				errs[i] = err
				switch filter {
				case "a":
					f.albums = hits
				case "b":
					f.artists = hits
				case "f":
					f.fans = hits
				}
			}(i, filter)
		}
		wg.Wait()
		if errs[0] != nil && errs[1] != nil && errs[2] != nil {
			return nil, errs[0]
		}
		return f, nil
	})
	if err != nil {
		return fail(err)
	}
	f := v.(found)
	var as []bandcamp.Album
	for _, h := range f.albums[:min(20, len(f.albums))] {
		as = append(as, bandcamp.Album{ID: h.ID, Type: "album", Title: h.Name, Artist: h.Artist, URL: h.URL, Art: h.Art})
	}
	items, live := d.albumItems(as, func(a bandcamp.Album) string { return joinNonEmpty(" · ", "Album", a.Artist) })
	for _, h := range f.artists[:min(12, len(f.artists))] {
		what := "Artist"
		if h.Label {
			what = "Label"
		}
		items = append(items, item{q2spot.Row{Kind: "list", URI: "artist", Context: h.URL, Title: h.Name,
			Subtitle: joinNonEmpty(" · ", what, h.Location, h.Genre)}, bandcamp.Pic{ID: h.Image}})
	}
	for _, h := range f.fans[:min(8, len(f.fans))] {
		items = append(items, item{q2spot.Row{Kind: "list", URI: "fan", Context: strconv.FormatInt(h.ID, 10), Title: h.Name,
			Subtitle: joinNonEmpty(" · ", "Fan", h.Username, "their playlists")}, bandcamp.Pic{ID: h.Image}})
	}
	if len(items) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing found", "for “"+text+"”")}}
	}
	return d.window(nil, items, "", liveMsg(live))
}

// artist is an artist's or a label's releases, as their music page lists them.
func (d *daemon) discography(root string) ([]bandcamp.Album, error) {
	v, err := d.cache.get("artist:"+strings.ToLower(root), 30*time.Minute, false, func(ctx context.Context) (any, error) {
		return d.bc.Discography(ctx, root)
	})
	if err != nil {
		return nil, err
	}
	return v.([]bandcamp.Album), nil
}

func (d *daemon) artist(root, cursor string) q2spot.Reply {
	if !strings.HasPrefix(root, "http") {
		return q2spot.Fail(q2spot.Bad, "Which artist?")
	}
	as, err := d.discography(root)
	if err != nil {
		return fail(err)
	}
	if len(as) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("No releases", "Bandcamp lists none for them")}}
	}
	items, live := d.albumItems(as, func(a bandcamp.Album) string {
		if a.Type == "track" {
			return joinNonEmpty(" · ", "Track", a.Artist)
		}
		return a.Artist
	})
	head := []item{{row: q2spot.Row{Kind: "action", URI: "getall", Context: root, Title: "Download All",
		Subtitle: plural(len(as), "release") + ", those not on the card"}}}
	return d.window(head, items, cursor, liveMsg(live))
}

// getAll downloads every release an artist's page lists (bandcamp-dl's --artist).
func (d *daemon) getAll(root string) q2spot.Reply {
	as, err := d.discography(root)
	if err != nil {
		return fail(err)
	}
	d.remember(as)
	added, had := 0, 0
	for _, a := range as {
		switch d.dl.add(albumJob(a), false) {
		case "added":
			added++
		case "done":
			had++
		}
	}
	if added == 0 {
		return okMsg("They're on the card or in Downloads already")
	}
	return okMsg("Downloading " + plural(added, "release"))
}

// retry queues a finished job again: its songs not on the card.
func (d *daemon) retry(key string) q2spot.Reply {
	d.dl.mu.Lock()
	old := d.dl.done[key]
	d.dl.mu.Unlock()
	if old == nil {
		return q2spot.Fail(q2spot.Unavailable, "It isn't in Downloads any more")
	}
	j := &job{Key: old.Key, Kind: old.Kind, URL: old.URL, ID: old.ID, Title: old.Title, Artist: old.Artist, Genre: old.Genre,
		Pic: old.Pic}
	if d.dl.add(j, true) == "queued" {
		return okMsg("It's in Downloads already")
	}
	return okMsg("Trying again")
}

func jobPic(j job) bandcamp.Pic {
	p, _ := bandcamp.ParsePic(j.Pic)
	return p
}

// downloadsPage is the queue: what downloads now (its bar), what waits (a press removes it), what
// finished (a press plays it from the card) and what failed (a press tries it again).
func (d *daemon) downloadsPage(cursor string) q2spot.Reply {
	s := d.dl.snap()
	var items []item
	if s.stalled != "" && len(s.queue) > 0 {
		items = append(items, item{row: info("Waiting: "+s.stalled, "It goes on by itself")})
	}
	for i, j := range s.queue {
		if i == 0 && s.cur {
			sub := "Asking Bandcamp for its songs"
			pml := 0
			if j.Total > 0 {
				sub = fmt.Sprintf("Song %d of %d: %s", s.songN, j.Total, s.song)
				part := 0.0
				if s.size > 0 {
					part = float64(s.got) / float64(s.size)
				}
				pml = int(1000 * (float64(j.Done+len(j.Failed)) + part) / float64(j.Total))
			}
			if q2.HasCap("progress") {
				items = append(items, item{q2spot.Row{Kind: "progress", Title: j.Title, Subtitle: sub,
					Position: strconv.Itoa(min(max(pml, 0), 1000))}, jobPic(j)})
			} else {
				items = append(items, item{info(j.Title, sub), jobPic(j)})
			}
			items = append(items, item{row: q2spot.Row{Kind: "action", URI: "cancel", Context: j.Key, Title: "Stop This Download",
				Subtitle: "The songs it has stay on the card"}})
			continue
		}
		items = append(items, item{q2spot.Row{Kind: "action", URI: "cancel", Context: j.Key, Title: j.Title,
			Subtitle: joinNonEmpty(" · ", j.Artist, "waiting: press to remove")}, jobPic(j)})
	}
	if len(s.queue) > 2 {
		items = append(items, item{row: q2spot.Row{Kind: "action", URI: "cancelall", Title: "Remove All Waiting",
			Subtitle: plural(len(s.queue)-1, "download")}})
	}
	clean := 0
	for _, j := range s.recent {
		switch {
		case j.Err != "":
			items = append(items, item{q2spot.Row{Kind: "action", URI: "retry", Context: j.Key, Title: j.Title,
				Subtitle: "Couldn't download: " + j.Err + " · press to try again"}, jobPic(j)})
		case len(j.Failed) > 0:
			items = append(items, item{q2spot.Row{Kind: "action", URI: "retry", Context: j.Key, Title: j.Title,
				Subtitle: fmt.Sprintf("%d of %d on the card · %d failed (%s) · press to try again", j.Done, j.Total,
					len(j.Failed), j.Failed[0])}, jobPic(j)})
		default:
			clean++
			sub := plural(j.Done, "song") + " downloaded"
			if j.Missing > 0 {
				sub += fmt.Sprintf(" · %d Bandcamp doesn't stream", j.Missing)
			}
			items = append(items, item{q2spot.Row{Kind: "local", URI: filepath.Join(d.root, j.Dir) + "/", Context: "album",
				Title: j.Title, Subtitle: joinNonEmpty(" · ", j.Artist, sub)}, jobPic(j)})
		}
	}
	if clean > 0 {
		items = append(items, item{row: q2spot.Row{Kind: "action", URI: "forget", Title: "Clear Finished",
			Subtitle: "From this list; they stay on the card"}})
	}
	if len(items) == 0 {
		items = append(items, item{row: info("Nothing to download", "Press an album to download it")})
	}
	return d.window(nil, items, cursor, "refresh")
}

// anyOnCard is whether a finished job's folder is on the card.
func (dl *downloads) anyOnCard() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	for _, j := range dl.done {
		if j.Err == "" && dl.onCard(j) {
			return true
		}
	}
	return false
}

// cardPage is everything downloaded that is still on the card, the newest first.
func (d *daemon) cardPage(cursor string) q2spot.Reply {
	d.dl.mu.Lock()
	var js []job
	for _, j := range d.dl.done {
		if j.Err == "" && d.dl.onCard(j) {
			js = append(js, *j)
		}
	}
	d.dl.mu.Unlock()
	sort.Slice(js, func(i, k int) bool { return js[i].Finished.After(js[k].Finished) })
	var items []item
	for _, j := range js {
		what := "Album"
		switch j.Kind {
		case "playlist":
			what = "Playlist"
		case "radio":
			what = "Radio show"
		}
		items = append(items, item{q2spot.Row{Kind: "local", URI: filepath.Join(d.root, j.Dir) + "/", Context: "album",
			Title: j.Title, Subtitle: joinNonEmpty(" · ", what, j.Artist, plural(j.Done, "song"))}, jobPic(j)})
	}
	if len(items) == 0 {
		items = append(items, item{row: info("Nothing downloaded yet", "Press an album to download it")})
	}
	return d.window(nil, items, cursor, "")
}
