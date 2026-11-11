package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/q2"
)

// The record: every release, playlist and radio show downloaded, kept in .bandcamp/done.json and
// written out for people as Bandcamp/Downloaded.txt, tab-separated (a spreadsheet opens it), oldest
// first: kind, title, artist, songs, when it was downloaded, when it left the card, its folder and
// its Bandcamp page. An album deleted from the card (here, or by hand: a sweep notices its folder
// gone) stays in the record, marked deleted, so a list's Download the Top N and an artist's
// Download All pass it over; a press on its own row downloads it again.

const recordName = "Downloaded.txt"

// recID is a record's short id, for a row's op: 12 hex digits of SHA-1 of its key.
func recID(key string) string {
	h := sha1.Sum([]byte(key))
	return hex.EncodeToString(h[:6])
}

// byID is the record with that id.
func (dl *downloads) byID(id string) (*job, bool) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	for k, j := range dl.done {
		if recID(k) == id {
			c := *j
			return &c, true
		}
	}
	return nil, false
}

// writeRecord writes Bandcamp/Downloaded.txt from the record; the caller holds mu.
func (dl *downloads) writeRecord() {
	var js []*job
	for _, j := range dl.done {
		if j.Done > 0 {
			js = append(js, j)
		}
	}
	if len(js) == 0 {
		return
	}
	sort.Slice(js, func(a, b int) bool { return js[a].Finished.Before(js[b].Finished) })
	clean := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	when := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	var b strings.Builder
	b.WriteString("Kind\tTitle\tArtist\tSongs\tDownloaded\tDeleted\tFolder\tLink\r\n")
	for _, j := range js {
		songs := fmt.Sprint(j.Done)
		if j.Total > 0 && j.Done != j.Total {
			songs = fmt.Sprintf("%d of %d", j.Done, j.Total)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\r\n", kindName(j.Kind), clean(j.Title), clean(j.Artist), songs,
			when(j.Finished), when(j.Deleted), filepath.ToSlash(j.Dir), link(j))
	}
	if !q2.CardMounted() || os.MkdirAll(dl.root, 0o755) != nil {
		return
	}
	if err := q2.WriteFile(filepath.Join(dl.root, recordName), []byte(b.String())); err != nil {
		logf("couldn't write %s: %v", recordName, err)
	}
}

func kindName(k string) string {
	switch k {
	case "playlist":
		return "Playlist"
	case "radio":
		return "Radio show"
	}
	return "Album"
}

// link is a record's Bandcamp page.
func link(j *job) string {
	switch {
	case j.URL != "":
		return j.URL
	case j.Kind == "radio":
		return fmt.Sprintf("https://bandcamp.com/radio?show=%d", j.ID)
	}
	return ""
}

// sweep marks deleted the records whose folders have left the card (deleted by hand), when the
// card is there to tell.
func (dl *downloads) sweep() {
	if !q2.CardMounted() {
		return
	}
	if _, err := os.Stat(q2.Card); err != nil {
		return
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	changed := false
	for _, j := range dl.done {
		if j.Done > 0 && j.Deleted.IsZero() && !dl.busyKey(j.Key) && !dl.onCard(j) {
			j.Deleted = time.Now()
			changed = true
			logf("record: %q is no longer on the card", j.Title)
		}
	}
	if changed {
		dl.saveDone()
	}
}

// busyKey is whether key is queued or under way; the caller holds mu.
func (dl *downloads) busyKey(key string) bool {
	for _, j := range dl.queue {
		if j.Key == key {
			return true
		}
	}
	return false
}

var errBusy = errors.New("it's downloading")

// delete removes a record's folder from the card: the songs and files the app wrote there (another
// file put in it is left, and so the folder), then its artist's folder if that is left empty. The
// record stays, marked deleted.
func (dl *downloads) delete(key string) (*job, error) {
	if !q2.CardMounted() {
		return nil, q2.ErrNoCard
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	j := dl.done[key]
	if j == nil || j.Done == 0 {
		return nil, errors.New("it isn't downloaded")
	}
	if dl.busyKey(key) {
		return nil, errBusy
	}
	dir := filepath.Join(dl.root, j.Dir)
	if rel, err := filepath.Rel(dl.root, dir); err != nil || j.Dir == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("its folder %q isn't in Bandcamp/", j.Dir)
	}
	files, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	n := 0
	for _, f := range files {
		name := f.Name()
		low := strings.ToLower(name)
		if f.IsDir() || !(strings.HasSuffix(low, ".mp3") || strings.HasSuffix(low, ".part") || name == "cover.jpg" ||
			name == ".q2-playlist") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
		n++
	}
	os.Remove(dir) // only when nothing else is in it
	if parent := filepath.Dir(dir); parent != dl.root && strings.HasPrefix(parent, dl.root+string(filepath.Separator)) {
		os.Remove(parent) // the artist's, Playlists or Radio, when it was the last
	}
	j.Deleted = time.Now()
	dl.saveDone()
	var keep []string
	for _, k := range dl.recent {
		if k != key {
			keep = append(keep, k)
		}
	}
	dl.recent = keep
	logf("deleted %q from the card: %d files in %s", j.Title, n, j.Dir)
	c := *j
	return &c, nil
}

func exists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}
