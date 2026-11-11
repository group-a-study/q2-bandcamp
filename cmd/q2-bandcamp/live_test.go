package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/bandcamp"
	"github.com/group-a-study/q2-bandcamp/internal/q2"
	"github.com/group-a-study/q2-bandcamp/internal/q2spot"
)

// TestLiveDaemon runs the app against the real Bandcamp, with its own TLS roots, on a card in
// Q2_BANDCAMP_LIVE (a folder): Album of the Day's latest is downloaded, a radio show's songs are
// kept as an album, the album is deleted, and the record says so. Q2_BANDCAMP_LIVE=<dir> go test
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
	page := aotd.Rows[2].Context
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
	// the record, and a delete
	ask("card")
	id := recID(releaseKey(page))
	ask("rec", id)
	ask("delete", id)
	ask("delete", id)
	ask("card")
	rec, _ := os.ReadFile(filepath.Join(card, "Bandcamp", recordName))
	t.Logf("%s:\n%s", recordName, rec)
	if st, _ := d.dl.state(releaseKey(page)); st != "gone" {
		t.Fatalf("after the delete: %s", st)
	}
	ask("aotd")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func qreq(op string, args ...string) q2spot.Request {
	return q2spot.Request{ID: "1", Op: op, Args: args}
}
