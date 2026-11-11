package bandcamp

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive asks the real Bandcamp: Q2_BANDCAMP_LIVE=1 go test -run Live -v ./internal/bandcamp
func TestLive(t *testing.T) {
	if os.Getenv("Q2_BANDCAMP_LIVE") == "" {
		t.Skip("Q2_BANDCAMP_LIVE unset")
	}
	c := New(http.DefaultTransport)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	nn, err := c.NewAndNotable(ctx, 120)
	if err != nil || len(nn) < 50 {
		t.Fatalf("new and notable: %d %v", len(nn), err)
	}
	for _, a := range nn[:3] {
		t.Logf("N&N %s · %s · %s · %s · %s · art %d", a.Title, a.Artist, a.Genre, a.Date.Format("2 Jan"), a.URL, a.Art)
	}
	aotd, err := c.AlbumsOfTheDay(ctx, 10)
	if err != nil || len(aotd) != 10 {
		t.Fatalf("aotd: %d %v", len(aotd), err)
	}
	for _, a := range aotd[:3] {
		t.Logf("AOTD %s · %s · %s · %s · %s", a.Title, a.Artist, a.Date.Format("2 Jan"), a.URL, a.Review)
	}
	r, err := c.Release(ctx, aotd[0].URL)
	if err != nil || len(r.Tracks) == 0 {
		t.Fatalf("release %s: %v", aotd[0].URL, err)
	}
	t.Logf("release %s · %s · label %q · %s · %d tracks (%d missing) · tags %v", r.Title, r.Artist, r.Label,
		r.Date.Format("2006-01-02"), len(r.Tracks), r.Missing, r.Tags)
	for _, tr := range r.Tracks[:min(3, len(r.Tracks))] {
		t.Logf("  %d %s · %s · %.0f s · lyrics %d · %s", tr.Num, tr.Title, tr.Artist, tr.Seconds, len(tr.Lyrics), tr.URL)
	}
	tl, err := c.Tracklist(ctx, "album", r.ID)
	if err != nil || len(tl.Tracks) == 0 || tl.Tracks[0].Stream == "" {
		t.Fatalf("album tracklist: %+v %v", tl, err)
	}
	pls, err := c.FeaturedPlaylists(ctx)
	if err != nil || len(pls) == 0 {
		t.Fatalf("featured playlists: %v", err)
	}
	t.Logf("%d featured playlists; first %s by %s, %d songs", len(pls), pls[0].Title, pls[0].Curator, pls[0].Count)
	pl, err := c.Tracklist(ctx, "playlist", pls[0].ID)
	if err != nil || len(pl.Tracks) == 0 {
		t.Fatalf("playlist: %v", err)
	}
	t.Logf("playlist %s: %d of %d tracks; first %s · %s · %s", pl.Title, len(pl.Tracks), pl.Total, pl.Tracks[0].Title,
		pl.Tracks[0].Artist, pl.Tracks[0].Album)
	shows, err := c.RadioShows(ctx)
	if err != nil || len(shows) < 900 {
		t.Fatalf("radio: %d %v", len(shows), err)
	}
	series := map[string]int{}
	for _, s := range shows {
		series[s.Series]++
	}
	t.Logf("%d shows: %v", len(shows), series)
	show, err := c.Tracklist(ctx, "radio", shows[0].ID)
	if err != nil || show.Stream == "" || len(show.Tracks) == 0 {
		t.Fatalf("show: %+v %v", show, err)
	}
	t.Logf("show %s / %s: %d tracks, %.0f s, at %v", show.Title, show.Subtitle, len(show.Tracks), show.Seconds,
		show.Tracks[1].At)
	resp, err := c.Open(ctx, show.Stream, "bytes=0-1023")
	if err != nil {
		t.Fatalf("show stream: %v", err)
	}
	resp.Body.Close()
	t.Logf("show stream: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Range"), resp.Request.URL.Host)
	hits, err := c.Search(ctx, "loves arp", "a")
	if err != nil || len(hits) == 0 || hits[0].Kind != "a" {
		t.Fatalf("search: %v %v", hits, err)
	}
	t.Logf("search: %s · %s · %s", hits[0].Name, hits[0].Artist, hits[0].URL)
	fans, err := c.Search(ctx, "funkentechno", "f")
	if err != nil || len(fans) == 0 {
		t.Fatalf("fans: %v", err)
	}
	fp, err := c.FanPlaylists(ctx, fans[0].ID)
	if err != nil {
		t.Fatalf("fan playlists: %v", err)
	}
	t.Logf("fan %s (%s): %d playlists", fans[0].Name, fans[0].Username, len(fp))
	f, err := c.FanByName(ctx, "funkentechno")
	if err != nil || f.ID != fans[0].ID {
		t.Fatalf("fan by name: %+v %v", f, err)
	}
	disc, err := c.Discography(ctx, "https://dawuna.bandcamp.com")
	if err != nil || len(disc) < 3 {
		t.Fatalf("discography: %v %v", disc, err)
	}
	for _, a := range disc[:3] {
		t.Logf("disc %s · %s · %s · art %d", a.Title, a.Artist, a.URL, a.Art)
	}
	tr, err := c.Release(ctx, "https://dawuna.bandcamp.com/track/love-jaunt-2")
	if err != nil || tr.Type != "track" || !strings.HasSuffix(tr.Album, "/album/loves-arp") {
		t.Fatalf("track page: %+v %v", tr, err)
	}
	pic, err := c.Picture(ctx, Pic{ID: r.Art, Art: true}, 3)
	if err != nil || len(pic) < 1000 {
		t.Fatalf("picture: %v", err)
	}
}
