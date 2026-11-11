package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
)

// TestLiveDaemon runs the app against the real Bandcamp, with its own TLS roots, on a card in
// Q2_BANDCAMP_LIVE (a folder): Album of the Day's latest is downloaded, a radio show's songs are
// kept as an album, and the proxy plays a song and a broadcast. Q2_BANDCAMP_LIVE=<dir> go test
// -run LiveDaemon -v ./cmd/q2-bandcamp
func TestLiveDaemon(t *testing.T) {
	base := os.Getenv("Q2_BANDCAMP_LIVE")
	if base == "" || !filepath.IsAbs(base) {
		t.Skip("Q2_BANDCAMP_LIVE unset")
	}
	bandcamp.Site, bandcamp.ImageHost = "https://bandcamp.com", "https://f4.bcbits.com"
	card := filepath.Join(base, "card")
	tmp := filepath.Join(base, "t", "q2-bandcamp")
	os.MkdirAll(filepath.Join(card, ".bandcamp"), 0o755)
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	caps := filepath.Join(base, "caps")
	os.WriteFile(caps, []byte(allCaps+"\n"), 0o644)
	q2.Card, q2.CapsFile = card, caps
	q2.CardMounted = func() bool { return true }
	logPath = filepath.Join(card, "q2-bandcamp.log")
	proxyPort = 0
	d := start(filepath.Join(card, ".bandcamp"), filepath.Join(card, "Bandcamp"), tmp)
	done := make(chan struct{})
	go func() { d.run(); close(done) }()
	defer func() { d.shutdown(); <-done }()
	ask := func(op string, args ...string) string {
		rep := d.answer(qreq(op, args...))
		t.Logf("%s %v → %s %q\n%s", op, args, rep.Status, rep.Message, rowsOf(rep))
		return rep.Message
	}
	waitIdle := func() {
		end := time.Now().Add(10 * time.Minute)
		for d.dl.pending() && time.Now().Before(end) {
			time.Sleep(time.Second)
		}
	}
	ask("menu")
	aotd := d.answer(qreq("aotd"))
	if len(aotd.Rows) < 10 {
		t.Fatalf("aotd: %+v", aotd)
	}
	t.Logf("aotd head:\n%s", rowsOf(aotd)[:600])
	page := aotd.Rows[1].Context
	ask("get", page)
	shows, err := d.shows()
	if err != nil {
		t.Fatal(err)
	}
	show := shows[0].ID
	ask("show", itoa(show))
	ask("getlist", "radio:"+itoa(show))
	ask("downloads")
	waitIdle()
	ask("downloads")
	ask("card")
	// the proxy: a real song and the broadcast, a bit of each
	ask("play", page)
	b, _ := os.ReadFile(tmp + ".urls")
	line := strings.Split(string(b), "\n")[1]
	url := strings.Split(line, "\t")[0]
	t.Logf("first line: %q", line)
	if strings.HasPrefix(url, "http") {
		get(t, url, "bytes=0-65535")
	}
	ask("playshow", itoa(show))
	b, _ = os.ReadFile(tmp + ".urls")
	get(t, strings.Split(strings.Split(string(b), "\n")[1], "\t")[0], "bytes=1000000-1065535")
	get(t, strings.Split(strings.Split(string(b), "\n")[1], "\t")[4], "")
}

func get(t *testing.T, url, rng string) {
	req, _ := http.NewRequest("GET", url, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	t.Logf("GET %s (%s): %d, %s, %d bytes", url, rng, resp.StatusCode, resp.Header.Get("Content-Range"), n)
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		t.Fatalf("proxy answered %d", resp.StatusCode)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func qreq(op string, args ...string) q2spot.Request {
	return q2spot.Request{ID: "1", Op: op, Args: args}
}
