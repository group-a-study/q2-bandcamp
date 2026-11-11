// Package bandcamp reads what q2-bandcamp shows and downloads from bandcamp.com, as its own pages do,
// with no account: the editors' New and Notable picks and Bandcamp Daily's Album of the Day
// (editorial/3), featured and fans' playlists (playlist/1, fan_collection/1), Bandcamp Radio's shows
// (radio_api/1, bcweekly/3), search (bcsearch_public_api/1), any release's or list's tracks with their
// stream URLs (player/2/player_data_web), and a release's own page, read as bandcamp-dl reads it: the
// data-tralbum attribute (each track's mp3-128 stream, number, artist and lyrics) and the ld+json
// script (tags, label). Stream URLs are the 128 kbps MP3s Bandcamp's own player plays; they last a
// day (ts= is their end).
//
// Bandcamp's front page answers a client that calls itself a browser but runs no JavaScript with a
// challenge page; this package says what it is (UA), as bandcamp-dl does, and gets the pages.
package bandcamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Site is bandcamp.com; ImageHost serves the covers. Tests point them at a fake.
var (
	Site      = "https://bandcamp.com"
	ImageHost = "https://f4.bcbits.com"
)

// UA is the User-Agent every request carries: the app's own name, as bandcamp-dl sends its own.
var UA = "q2-bandcamp/1.0 (+https://github.com/group-a-study/q2-bandcamp)"

// Client asks Bandcamp. HTTP carries every request (the app's TLS roots).
type Client struct {
	HTTP *http.Client
}

func New(rt http.RoundTripper) *Client {
	return &Client{HTTP: &http.Client{Transport: rt, Timeout: 60 * time.Second}}
}

var (
	ErrNotFound = errors.New("not on Bandcamp any more")
	ErrBlocked  = errors.New("Bandcamp asked for a browser check")
)

// StatusError is a reply Bandcamp gave that isn't a page or JSON it meant.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("Bandcamp answered %d", e.Code) }

// IsNoNetwork is whether err is the network's (no Wi-Fi, a name that didn't resolve, a dropped or
// timed-out connection, Bandcamp's servers busy) rather than Bandcamp refusing what was asked.
func IsNoNetwork(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == 429 || se.Code >= 500
	}
	var ne net.Error
	var de *net.DNSError
	var oe *net.OpError
	return errors.As(err, &ne) || errors.As(err, &de) || errors.As(err, &oe) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "broken pipe")
}

const maxPage = 8 << 20 // a page or a reply read this far at most

func (c *Client) do(req *http.Request) ([]byte, string, error) {
	req.Header.Set("User-Agent", UA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return nil, "", err
	}
	switch {
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return nil, "", ErrNotFound
	case resp.StatusCode != 200:
		return nil, "", &StatusError{resp.StatusCode}
	case bytes.Contains(b[:min(len(b), 4096)], []byte("<title>Client Challenge</title>")):
		return nil, "", ErrBlocked
	}
	return b, resp.Request.URL.String(), nil
}

// page is a page's HTML and the address it ended at (after redirects).
func (c *Client) page(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	return c.do(req)
}

// api posts body as JSON to Bandcamp's /api/<path> and decodes its answer into out. An answer
// that is an error of Bandcamp's ({"error": true} or {"__api_special__": "exception"}) is one here.
func (c *Client) api(ctx context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", Site+"/api/"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	return c.decode(req, out)
}

// get is a GET of Bandcamp's /api/<path>, decoded into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", Site+"/api/"+path, nil)
	if err != nil {
		return err
	}
	return c.decode(req, out)
}

func (c *Client) decode(req *http.Request, out any) error {
	b, _, err := c.do(req)
	if err != nil {
		return err
	}
	var e struct {
		Error   any    `json:"error"`
		Message string `json:"error_message"`
		Special string `json:"__api_special__"`
		Type    string `json:"error_type"`
	}
	if json.Unmarshal(b, &e) == nil && (e.Error == true || e.Special == "exception") {
		if strings.Contains(e.Type, "NotFound") || strings.Contains(e.Message, "not found") {
			return ErrNotFound
		}
		return fmt.Errorf("Bandcamp refused %s: %s%s", strings.TrimPrefix(req.URL.Path, "/api/"), e.Message, e.Type)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("Bandcamp's %s: %w", strings.TrimPrefix(req.URL.Path, "/api/"), err)
	}
	return nil
}

// ArtURL is a release's cover (its art id) at one of Bandcamp's sizes: 3 is 100 px, 7 150 px, 2
// 350 px, 16 700 px, 10 1200 px. ImageURL is any other picture (a show's, a playlist's, a
// person's), by its image id, at the same sizes.
func ArtURL(artID int64, size int) string {
	return fmt.Sprintf("%s/img/a%010d_%d.jpg", ImageHost, artID, size)
}

func ImageURL(imageID int64, size int) string {
	return fmt.Sprintf("%s/img/%010d_%d.jpg", ImageHost, imageID, size)
}

// Pic is a picture: a release's cover (Art) or another image. Zero is none.
type Pic struct {
	ID  int64
	Art bool
}

func (p Pic) URL(size int) string {
	if p.Art {
		return ArtURL(p.ID, size)
	}
	return ImageURL(p.ID, size)
}

// Key names the picture in a file name: a<id> or i<id>.
func (p Pic) Key() string {
	if p.Art {
		return "a" + strconv.FormatInt(p.ID, 10)
	}
	return "i" + strconv.FormatInt(p.ID, 10)
}

// ParsePic reverses Key.
func ParsePic(k string) (Pic, bool) {
	if len(k) < 2 || (k[0] != 'a' && k[0] != 'i') {
		return Pic{}, false
	}
	id, err := strconv.ParseInt(k[1:], 10, 64)
	return Pic{ID: id, Art: k[0] == 'a'}, err == nil && id > 0
}

// Picture fetches a picture's JPEG at size.
func (c *Client) Picture(ctx context.Context, p Pic, size int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL(size), nil)
	if err != nil {
		return nil, err
	}
	b, _, err := c.do(req)
	if err == nil && (len(b) < 4 || b[0] != 0xff || b[1] != 0xd8) {
		err = errors.New("the picture isn't a JPEG")
	}
	return b, err
}

// Bandcamp's dates: "10 Nov 2026 00:00:00 GMT".
func parseDate(s string) time.Time {
	t, _ := time.Parse("02 Jan 2006 15:04:05 MST", s)
	return t
}

// Album is a release as a list shows it: an album, or a track sold alone (Type "track").
type Album struct {
	ID     int64
	Type   string // "album" or "track"
	Title  string
	Artist string
	URL    string // its page
	Art    int64  // art id
	Genre  string
	Blurb  string    // the editors' line about it
	Date   time.Time // the day it was picked (New and Notable, Album of the Day) or released
	Review string    // Bandcamp Daily's article (Album of the Day)
}

type attribution struct {
	Name        string `json:"name"`
	AccountType string `json:"accountType"`
}

type playerTrack struct {
	Album *struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		ArtID int64  `json:"artId"`
		URL   string `json:"url"`
	} `json:"album"`
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	ArtistName  string   `json:"artistName"`
	BandURL     string   `json:"bandUrl"`
	Duration    float64  `json:"duration"`
	ArtID       int64    `json:"artId"`
	URL         string   `json:"url"`
	StreamURL   string   `json:"streamUrl"`
	Disabled    bool     `json:"isStreamingDisabled"`
	Timecode    *float64 `json:"timecode"`
	IsPreorder  bool     `json:"isPreorder"`
	TrackNumber int      `json:"trackNumber"`
}

type editorialItem struct {
	Attribution attribution  `json:"attribution"`
	Date        string       `json:"date"`
	Description string       `json:"description"`
	Genre       string       `json:"genre"`
	ImageID     int64        `json:"imageId"`
	ItemID      int64        `json:"itemId"`
	ItemType    string       `json:"itemType"`
	ItemURL     string       `json:"itemUrl"`
	Title       string       `json:"title"`
	PlayerData  *playerTrack `json:"playerData"`
}

// album is an editorial pick as an Album: what it plays (its playerData) names the release.
func (it editorialItem) album(review bool) (Album, bool) {
	a := Album{Genre: it.Genre, Blurb: strings.TrimSpace(it.Description), Date: parseDate(it.Date), Art: it.ImageID}
	p := it.PlayerData
	switch {
	case p != nil && p.Album != nil && p.Album.URL != "":
		a.ID, a.Type, a.Title, a.URL = p.Album.ID, "album", p.Album.Title, p.Album.URL
		if p.Album.ArtID != 0 {
			a.Art = p.Album.ArtID
		}
	case p != nil && p.URL != "":
		a.ID, a.Type, a.Title, a.URL = p.ID, "track", p.Title, p.URL
		if p.ArtID != 0 {
			a.Art = p.ArtID
		}
	case !review && it.ItemURL != "":
		a.ID, a.Type, a.Title, a.URL = it.ItemID, it.ItemType, it.Title, it.ItemURL
	default:
		return a, false
	}
	if p != nil && p.ArtistName != "" {
		a.Artist = p.ArtistName
	} else if it.Attribution.AccountType == "band" {
		a.Artist = it.Attribution.Name
	}
	if review {
		a.Review = it.ItemURL
	} else if it.Attribution.AccountType == "band" && it.Attribution.Name != "" {
		a.Artist = it.Attribution.Name // the band that released it, over one track's artist
	}
	return a, true
}

// NewAndNotable is the editors' latest picks, newest first, up to n (Bandcamp keeps 120).
func (c *Client) NewAndNotable(ctx context.Context, n int) ([]Album, error) {
	return c.editorial(ctx, "get_new_and_notable", n, false)
}

// AlbumsOfTheDay is Bandcamp Daily's Album of the Day, the latest first, up to n.
func (c *Client) AlbumsOfTheDay(ctx context.Context, n int) ([]Album, error) {
	return c.editorial(ctx, "get_aotd", n, true)
}

func (c *Client) editorial(ctx context.Context, fn string, n int, review bool) ([]Album, error) {
	var out []Album
	var cursor any
	for len(out) < n {
		var r struct {
			Items      []editorialItem `json:"items"`
			NextCursor any             `json:"nextCursor"`
		}
		body := map[string]any{"page_size": n - len(out)}
		if cursor != nil {
			body["next_cursor"] = cursor
		}
		if err := c.api(ctx, "editorial/3/"+fn, body, &r); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		for _, it := range r.Items {
			if a, ok := it.album(review); ok {
				out = append(out, a)
			}
		}
		if len(r.Items) == 0 || r.NextCursor == nil || r.NextCursor == "" {
			break
		}
		cursor = r.NextCursor
	}
	return out, nil
}

// Playlist is a playlist as a list shows it.
type Playlist struct {
	ID       int64
	Title    string
	Curator  string
	URL      string
	Image    int64 // image id
	Count    int
	Seconds  float64
	Modified time.Time
}

type playlistItem struct {
	Attribution attribution `json:"attribution"`
	ImageID     int64       `json:"imageId"`
	ItemID      int64       `json:"itemId"`
	ItemURL     string      `json:"itemUrl"`
	ModDate     string      `json:"modDate"`
	Title       string      `json:"title"`
	IsPrivate   bool        `json:"isPrivate"`
	Summary     struct {
		Count    int     `json:"totalCount"`
		Duration float64 `json:"totalDuration"`
	} `json:"tracksSummary"`
}

func (p playlistItem) playlist() Playlist {
	return Playlist{ID: p.ItemID, Title: p.Title, Curator: p.Attribution.Name, URL: p.ItemURL, Image: p.ImageID,
		Count: p.Summary.Count, Seconds: p.Summary.Duration, Modified: parseDate(p.ModDate)}
}

// FeaturedPlaylists is the playlists Bandcamp's front page features.
func (c *Client) FeaturedPlaylists(ctx context.Context) ([]Playlist, error) {
	return c.playlists(ctx, "playlist/1/featured_playlists", map[string]any{})
}

// FanPlaylists is a fan's public playlists.
func (c *Client) FanPlaylists(ctx context.Context, fanID int64) ([]Playlist, error) {
	return c.playlists(ctx, "fan_collection/1/playlists", map[string]any{"page_fan_id": fanID})
}

func (c *Client) playlists(ctx context.Context, path string, body map[string]any) ([]Playlist, error) {
	var out []Playlist
	var cursor any
	for page := 0; page < 25; page++ {
		var r struct {
			Items      []playlistItem `json:"items"`
			NextCursor any            `json:"nextCursor"`
		}
		body["page_size"] = 20
		if cursor != nil {
			body["next_cursor"] = cursor
		} else {
			delete(body, "next_cursor")
		}
		if err := c.api(ctx, path, body, &r); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		for _, it := range r.Items {
			if !it.IsPrivate && it.ItemID != 0 {
				out = append(out, it.playlist())
			}
		}
		if len(r.Items) == 0 || r.NextCursor == nil || r.NextCursor == "" {
			break
		}
		cursor = r.NextCursor
	}
	return out, nil
}

// Show is a Bandcamp Radio show as its list shows it.
type Show struct {
	ID     int64
	Title  string // the episode's own name (a guest, a theme)
	Series string // Bandcamp Weekly, The Metal Show, …
	Date   time.Time
	Image  int64
	Blurb  string
}

// OtherShows is the series of a show Bandcamp names none for.
const OtherShows = "Other Shows"

// RadioShows is every Bandcamp Radio show, the newest first. Shows Bandcamp's radio list names no
// series for (older ones) take their series from the old Bandcamp Weekly list, where it has one.
func (c *Client) RadioShows(ctx context.Context) ([]Show, error) {
	var r struct {
		Items []struct {
			Date      string `json:"date"`
			Desc      string `json:"description"`
			ImageID   int64  `json:"imageId"`
			ItemID    int64  `json:"itemId"`
			Franchise string `json:"franchiseName"`
			Title     string `json:"title"`
		} `json:"items"`
	}
	if err := c.api(ctx, "radio_api/1/get_radio_shows", map[string]any{"page_size": 5000}, &r); err != nil {
		return nil, err
	}
	var old map[int64]string
	out := make([]Show, 0, len(r.Items))
	for _, it := range r.Items {
		s := Show{ID: it.ItemID, Title: strings.TrimSpace(it.Title), Series: Series(it.Franchise), Date: parseDate(it.Date),
			Image: it.ImageID, Blurb: strings.TrimSpace(it.Desc)}
		if s.Series == "" {
			if old == nil {
				old = c.weeklySeries(ctx)
			}
			s.Series = Series(old[s.ID])
		}
		if s.Series == "" {
			s.Series = OtherShows
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// weeklySeries is the old Bandcamp Weekly list's series name for each show id (empty on failure).
func (c *Client) weeklySeries(ctx context.Context) map[int64]string {
	var r struct {
		Results []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"results"`
	}
	m := map[int64]string{}
	if c.get(ctx, "bcweekly/3/list", &r) == nil {
		for _, s := range r.Results {
			m[s.ID] = s.Title
		}
	}
	return m
}

var numbered = regexp.MustCompile(`\s*№\s*\d+\s*$`)

// Series tidies a series' name: "The Hip-Hop Show" and "The Hip Hop Show" are one, and so are
// "Bandcamp Weekly № 12" and "Bandcamp Weekly".
func Series(s string) string {
	s = strings.Join(strings.Fields(numbered.ReplaceAllString(s, "")), " ")
	return strings.ReplaceAll(s, "Hip-Hop", "Hip Hop")
}

// Track is a song in a list or on a release.
type Track struct {
	ID       int64
	Num      int // its number on its release (a release's page), else its place in the list
	Title    string
	Artist   string
	Seconds  float64
	Stream   string // its mp3-128 URL, "" when Bandcamp won't stream it
	Lyrics   string
	URL      string // its own page
	Art      int64
	Album    string // the release it's on, for a list's track
	AlbumURL string
	At       float64 // where a radio show plays it, in seconds; -1 unknown
}

// Tracklist is a release's, a playlist's or a radio show's songs, from Bandcamp's player.
type Tracklist struct {
	Type     string // album, track, playlist, radio
	ID       int64
	Title    string
	Subtitle string // a show's series
	Artist   string // a release's artist, a playlist's curator
	Image    Pic
	Date     time.Time
	Tracks   []Track
	Total    int
	Stream   string // a radio show's whole broadcast
	Seconds  float64
}

// MaxTracks is the most a Tracklist takes in: a playlist can be thousands long.
const MaxTracks = 2000

// Tracklist is the list's songs with their stream URLs, every page of them.
func (c *Client) Tracklist(ctx context.Context, kind string, id int64) (*Tracklist, error) {
	tl := &Tracklist{Type: kind, ID: id}
	var cursor any
	for first := true; ; first = false {
		var r struct {
			Tracklist struct {
				Title       string        `json:"title"`
				Subtitle    string        `json:"subtitle"`
				ImageID     int64         `json:"imageId"`
				IsArt       bool          `json:"isArt"`
				Date        string        `json:"date"`
				Attribution *attribution  `json:"attribution"`
				Tracks      []playerTrack `json:"tracks"`
				Summary     struct {
					Count    int     `json:"totalCount"`
					Duration float64 `json:"totalDuration"`
				} `json:"tracksSummary"`
				Compiled *struct {
					StreamURL string  `json:"streamUrl"`
					Duration  float64 `json:"duration"`
				} `json:"compiledTrack"`
				NextCursor any `json:"nextCursor"`
			} `json:"tracklist"`
		}
		body := map[string]any{"item_type": kind, "item_id": id}
		if cursor != nil {
			body["next_cursor"] = cursor
		}
		if err := c.api(ctx, "player/2/player_data_web", body, &r); err != nil {
			if !first {
				break // what came is kept
			}
			return nil, err
		}
		t := r.Tracklist
		if first {
			tl.Title, tl.Subtitle, tl.Image, tl.Date = t.Title, t.Subtitle, Pic{t.ImageID, t.IsArt}, parseDate(t.Date)
			if t.Attribution != nil {
				tl.Artist = t.Attribution.Name
			}
			tl.Total = t.Summary.Count
			if t.Compiled != nil {
				tl.Stream, tl.Seconds = t.Compiled.StreamURL, t.Compiled.Duration
			}
		}
		for _, p := range t.Tracks {
			tr := Track{ID: p.ID, Num: len(tl.Tracks) + 1, Title: p.Title, Artist: p.ArtistName, Seconds: p.Duration,
				URL: p.URL, Art: p.ArtID, At: -1}
			if !p.Disabled {
				tr.Stream = p.StreamURL
			}
			if p.Album != nil {
				tr.Album, tr.AlbumURL = p.Album.Title, p.Album.URL
				if tr.Art == 0 {
					tr.Art = p.Album.ArtID
				}
			}
			if p.Timecode != nil {
				tr.At = *p.Timecode
			}
			tl.Tracks = append(tl.Tracks, tr)
		}
		if len(t.Tracks) == 0 || t.NextCursor == nil || t.NextCursor == "" || len(tl.Tracks) >= MaxTracks {
			break
		}
		cursor = t.NextCursor
	}
	if tl.Total < len(tl.Tracks) {
		tl.Total = len(tl.Tracks)
	}
	return tl, nil
}

// Release is a release read from its own page, as bandcamp-dl reads it.
type Release struct {
	ID      int64
	Type    string // album or track
	Title   string
	Artist  string
	Label   string
	URL     string
	Date    time.Time
	Art     int64
	Tags    []string
	Tracks  []Track // the ones Bandcamp streams
	Missing int     // tracks it lists but doesn't stream (sold only, or not out yet)
	Album   string  // a track page's album, when the track is on one
}

var (
	tralbumAttr = regexp.MustCompile(`data-tralbum="([^"]*)"`)
	ldJSON      = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
)

// Release reads an album or track page.
func (c *Client) Release(ctx context.Context, pageURL string) (*Release, error) {
	b, final, err := c.page(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	return ParseRelease(b, final)
}

// ParseRelease reads a release page's HTML; base is its address.
func ParseRelease(page []byte, base string) (*Release, error) {
	m := tralbumAttr.FindSubmatch(page)
	if m == nil {
		return nil, errors.New("the page has no release on it")
	}
	var t struct {
		Artist      string `json:"artist"`
		ItemType    string `json:"item_type"`
		ID          int64  `json:"id"`
		ArtID       int64  `json:"art_id"`
		URL         string `json:"url"`
		AlbumURL    string `json:"album_url"`
		ReleaseDate string `json:"album_release_date"`
		Current     struct {
			Title       string `json:"title"`
			ReleaseDate string `json:"release_date"`
			PublishDate string `json:"publish_date"`
		} `json:"current"`
		Tracks []struct {
			ID        int64             `json:"id"`
			TrackID   int64             `json:"track_id"`
			File      map[string]string `json:"file"`
			Artist    string            `json:"artist"`
			Title     string            `json:"title"`
			Num       *int              `json:"track_num"`
			TitleLink string            `json:"title_link"`
			HasLyrics bool              `json:"has_lyrics"`
			Lyrics    string            `json:"lyrics"`
			Duration  float64           `json:"duration"`
		} `json:"trackinfo"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &t); err != nil {
		return nil, fmt.Errorf("the release's data: %w", err)
	}
	r := &Release{ID: t.ID, Type: t.ItemType, Title: t.Current.Title, Artist: t.Artist, URL: cmpOr(t.URL, base),
		Art: t.ArtID}
	for _, d := range []string{t.ReleaseDate, t.Current.ReleaseDate, t.Current.PublishDate} {
		if r.Date = parseDate(d); !r.Date.IsZero() {
			break
		}
	}
	if t.AlbumURL != "" {
		r.Album = resolve(r.URL, t.AlbumURL)
	}
	if ld := ldJSON.FindSubmatch(page); ld != nil {
		var l struct {
			Keywords  any `json:"keywords"`
			Publisher struct {
				Name string `json:"name"`
			} `json:"publisher"`
			ByArtist struct {
				Name string `json:"name"`
			} `json:"byArtist"`
		}
		if json.Unmarshal(ld[1], &l) == nil {
			switch k := l.Keywords.(type) {
			case []any:
				for _, v := range k {
					if s, ok := v.(string); ok {
						r.Tags = append(r.Tags, s)
					}
				}
			case string:
				for _, s := range strings.Split(k, ",") {
					if s = strings.TrimSpace(s); s != "" {
						r.Tags = append(r.Tags, s)
					}
				}
			}
			if p := strings.TrimSpace(l.Publisher.Name); p != "" && !strings.EqualFold(p, r.Artist) {
				r.Label = p
			}
		}
	}
	if r.Title == "" && len(t.Tracks) > 0 {
		r.Title = t.Tracks[0].Title
	}
	seen := map[int]bool{}
	dup := false
	for _, x := range t.Tracks {
		if x.Num != nil {
			dup = dup || seen[*x.Num]
			seen[*x.Num] = true
		}
	}
	for i, x := range t.Tracks {
		id := x.TrackID
		if id == 0 {
			id = x.ID
		}
		tr := Track{ID: id, Num: i + 1, Artist: x.Artist, Seconds: x.Duration, Art: r.Art, At: -1}
		if x.Num != nil && *x.Num > 0 && !dup {
			tr.Num = *x.Num
		}
		// bandcamp-dl's own tidying: a compilation's "Artist - Title" loses the artist it names
		tr.Title = x.Title
		if x.Artist != "" {
			tr.Title = strings.Replace(x.Title, x.Artist+" - ", "", 1)
		}
		if x.TitleLink != "" {
			tr.URL = resolve(r.URL, x.TitleLink)
		}
		if x.HasLyrics || x.Lyrics != "" {
			tr.Lyrics = strings.ReplaceAll(strings.ReplaceAll(x.Lyrics, "\r\n", "\n"), `\r\n`, "\n")
		}
		s := x.File["mp3-128"]
		if strings.HasPrefix(s, "//") {
			s = "https:" + s
		}
		if s == "" {
			r.Missing++
			continue
		}
		tr.Stream = s
		r.Tracks = append(r.Tracks, tr)
	}
	if len(r.Tracks) == 0 && r.Missing == 0 {
		return nil, errors.New("the release has no tracks")
	}
	return r, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resolve is ref against base, as a browser would.
func resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := b.Parse(ref)
	if err != nil {
		return ref
	}
	return r.String()
}

// Hit is a search result.
type Hit struct {
	Kind     string // a album, b artist or label, f fan, t track
	ID       int64
	Name     string
	Artist   string // an album's or a track's
	URL      string // an album's page, an artist's or a fan's front page
	Art      int64
	Image    int64
	Location string
	Genre    string
	Label    bool
	Username string
	Album    string // a track's album
}

// Search is what Bandcamp's search box suggests for text: filter a for albums, b for artists and
// labels, f for fans, t for tracks; "" for any.
func (c *Client) Search(ctx context.Context, text, filter string) ([]Hit, error) {
	var r struct {
		Auto struct {
			Results []struct {
				Type       string `json:"type"`
				ID         int64  `json:"id"`
				ArtID      int64  `json:"art_id"`
				ImgID      int64  `json:"img_id"`
				ImageID    int64  `json:"image_id"`
				Name       string `json:"name"`
				BandName   string `json:"band_name"`
				AlbumName  string `json:"album_name"`
				URLRoot    string `json:"item_url_root"`
				URLPath    string `json:"item_url_path"`
				Location   string `json:"location"`
				IsLabel    bool   `json:"is_label"`
				GenreName  string `json:"genre_name"`
				Username   string `json:"username"`
				Collection int    `json:"collection_size"`
			} `json:"results"`
		} `json:"auto"`
	}
	if err := c.api(ctx, "bcsearch_public_api/1/autocomplete_elastic",
		map[string]any{"search_text": text, "search_filter": filter, "full_page": true, "fan_id": nil}, &r); err != nil {
		return nil, err
	}
	var out []Hit
	for _, x := range r.Auto.Results {
		h := Hit{Kind: x.Type, ID: x.ID, Name: x.Name, Artist: x.BandName, URL: cmpOr(x.URLPath, x.URLRoot), Art: x.ArtID,
			Image: x.ImgID, Location: x.Location, Genre: x.GenreName, Label: x.IsLabel, Username: x.Username, Album: x.AlbumName}
		if h.Image == 0 {
			h.Image = x.ImageID
		}
		if h.URL == "" || h.ID == 0 {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// Fan is a fan's page as their playlists need it.
type Fan struct {
	ID       int64
	Username string
	Name     string
}

var blobAttr = regexp.MustCompile(`data-blob="([^"]*)"`)

// FanByName reads a fan's page, bandcamp.com/<username>.
func (c *Client) FanByName(ctx context.Context, username string) (*Fan, error) {
	b, _, err := c.page(ctx, Site+"/"+url.PathEscape(username))
	if err != nil {
		return nil, err
	}
	for _, m := range blobAttr.FindAllSubmatch(b, -1) {
		var blob struct {
			FanData *struct {
				FanID    int64  `json:"fan_id"`
				Username string `json:"username"`
				Name     string `json:"name"`
			} `json:"fan_data"`
		}
		if json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &blob) == nil && blob.FanData != nil &&
			blob.FanData.FanID != 0 {
			return &Fan{ID: blob.FanData.FanID, Username: blob.FanData.Username, Name: blob.FanData.Name}, nil
		}
	}
	return nil, ErrNotFound
}

var (
	gridItem    = regexp.MustCompile(`(?s)<li[^>]*data-item-id="(album|track)-(\d+)"[^>]*>(.*?)</li>`)
	gridHref    = regexp.MustCompile(`<a href="([^"]+)"`)
	gridArt     = regexp.MustCompile(`(?:data-original|src)="[^"]*/img/a0*(\d+)_\d+\.jpg"`)
	gridTitle   = regexp.MustCompile(`(?s)<p class="title">(.*?)</p>`)
	gridArtist  = regexp.MustCompile(`(?s)<span class="artist-override">(.*?)</span>`)
	clientItems = regexp.MustCompile(`data-client-items="([^"]*)"`)
	tagRE       = regexp.MustCompile(`<[^>]*>`)
)

func text(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(s, " "))), " ")
}

// Discography is an artist's or a label's releases, as their music page lists them (bandcamp-dl's
// --artist): its grid, and the rest of a long grid in its data-client-items. A page that is one
// release (an artist with one) is that release.
func (c *Client) Discography(ctx context.Context, root string) ([]Album, error) {
	root = strings.TrimRight(root, "/")
	b, final, err := c.page(ctx, root+"/music")
	if err != nil {
		return nil, err
	}
	artist := ""
	if m := regexp.MustCompile(`(?s)<p id="band-name-location">\s*<span class="title">(.*?)</span>`).FindSubmatch(b); m != nil {
		artist = text(string(m[1]))
	}
	var out []Album
	seen := map[string]bool{}
	add := func(a Album) {
		if a.URL == "" || seen[a.URL] {
			return
		}
		seen[a.URL] = true
		if a.Artist == "" {
			a.Artist = artist
		}
		out = append(out, a)
	}
	for _, m := range gridItem.FindAllSubmatch(b, -1) {
		body := string(m[3])
		h := gridHref.FindStringSubmatch(body)
		if h == nil {
			continue
		}
		id, _ := strconv.ParseInt(string(m[2]), 10, 64)
		a := Album{ID: id, Type: string(m[1]), URL: resolve(final, html.UnescapeString(h[1]))}
		if t := gridTitle.FindStringSubmatch(body); t != nil {
			if ar := gridArtist.FindStringSubmatch(t[1]); ar != nil {
				a.Artist = text(ar[1])
				t[1] = strings.Replace(t[1], ar[0], "", 1)
			}
			a.Title = text(t[1])
		}
		if art := gridArt.FindStringSubmatch(body); art != nil {
			a.Art, _ = strconv.ParseInt(art[1], 10, 64)
		}
		add(a)
	}
	if m := clientItems.FindSubmatch(b); m != nil {
		var items []struct {
			ID       int64  `json:"id"`
			Type     string `json:"type"`
			Title    string `json:"title"`
			Artist   string `json:"artist"`
			PageURL  string `json:"page_url"`
			ArtID    int64  `json:"art_id"`
			BandName string `json:"band_name"`
		}
		if json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &items) == nil {
			for _, it := range items {
				add(Album{ID: it.ID, Type: it.Type, Title: it.Title, Artist: cmpOr(it.Artist, it.BandName),
					URL: resolve(final, it.PageURL), Art: it.ArtID})
			}
		}
	}
	if len(out) == 0 {
		if r, err := ParseRelease(b, final); err == nil {
			out = append(out, Album{ID: r.ID, Type: r.Type, Title: r.Title, Artist: r.Artist, URL: r.URL, Art: r.Art,
				Date: r.Date})
		}
	}
	return out, nil
}

// Open GETs a stream URL (following Bandcamp's stream_redirect), with the Range header rng when
// it isn't "". The caller closes the body. 200 and 206 are answers; anything else is an error.
func (c *Client) Open(ctx context.Context, streamURL, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UA)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	cl := *c.HTTP
	cl.Timeout = 0 // a long show streams for an hour and more; the caller's context ends it
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		if resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 410 {
			return nil, ErrExpired
		}
		return nil, &StatusError{resp.StatusCode}
	}
	return resp, nil
}

// ErrExpired is a stream URL Bandcamp no longer takes: past its day, or the track gone. Asking for
// the list again gives a new one.
var ErrExpired = errors.New("the stream address expired")
