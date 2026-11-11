// Package id3 writes the ID3v2.4 tag q2-bandcamp puts on each downloaded MP3, as bandcamp-dl does
// with mutagen: title, artist, album artist, album, track number, year, genre, label, the release's
// page (WOAF), lyrics (USLT) and the cover (APIC); and, for a playlist's or a radio show's songs kept
// as an album, the compilation flag (TCMP). Text is UTF-8. Bandcamp's MP3s carry no tag of their
// own, only LAME's Info frame, which stays first in the audio for gapless playback.
package id3

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
)

// Tags is what a song's tag says.
type Tags struct {
	Title, Artist, AlbumArtist, Album string
	Track, Tracks                     int // 0: none
	Year                              string
	Genre, Label, URL, Lyrics         string
	Compilation                       bool
	Cover                             []byte // a JPEG
}

const padding = 1024 // room for a tag editor's later change

// Build is the whole tag.
func (t Tags) Build() []byte {
	var f bytes.Buffer
	text := func(id, s string) {
		if s != "" {
			frame(&f, id, append([]byte{3}, s...))
		}
	}
	text("TIT2", t.Title)
	text("TPE1", t.Artist)
	text("TPE2", t.AlbumArtist)
	text("TALB", t.Album)
	if t.Track > 0 {
		n := strconv.Itoa(t.Track)
		if t.Tracks > 0 {
			n += "/" + strconv.Itoa(t.Tracks)
		}
		text("TRCK", n)
	}
	text("TDRC", t.Year)
	text("TCON", t.Genre)
	text("TPUB", t.Label)
	if t.Compilation {
		text("TCMP", "1")
	}
	if t.URL != "" {
		frame(&f, "WOAF", []byte(t.URL)) // a URL frame: ISO-8859-1, no encoding byte
	}
	if t.Lyrics != "" {
		frame(&f, "USLT", append([]byte{3, 'e', 'n', 'g', 0}, t.Lyrics...)) // UTF-8, English, no description
	}
	if len(t.Cover) > 0 {
		apic := append([]byte{0}, "image/jpeg"...)
		apic = append(apic, 0, 3, 0) // the MIME type's end, a front cover, no description
		frame(&f, "APIC", append(apic, t.Cover...))
	}
	f.Write(make([]byte, padding))
	out := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
	putSyncsafe(out[6:10], f.Len())
	return append(out, f.Bytes()...)
}

func frame(w *bytes.Buffer, id string, body []byte) {
	h := make([]byte, 10)
	copy(h, id)
	putSyncsafe(h[4:8], len(body))
	w.Write(h)
	w.Write(body)
}

func putSyncsafe(b []byte, n int) {
	b[0], b[1], b[2], b[3] = byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f)
}

func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// SkipTags passes any ID3v2 tags at r's start, leaving r at the audio (the new tag replaces them).
func SkipTags(r *bufio.Reader) error {
	for {
		b, _ := r.Peek(10)
		if len(b) < 10 || string(b[:3]) != "ID3" {
			return nil
		}
		n := syncsafe(b[6:10]) + 10
		if b[3] == 4 && b[5]&0x10 != 0 {
			n += 10 // a footer
		}
		if _, err := r.Discard(n); err != nil {
			return err
		}
	}
}

// Write is the tag then src's audio (any tag it had passed over), to w.
func Write(w io.Writer, src io.Reader, t Tags) error {
	r := bufio.NewReaderSize(src, 64<<10)
	if err := SkipTags(r); err != nil {
		return err
	}
	if _, err := w.Write(t.Build()); err != nil {
		return err
	}
	_, err := io.CopyBuffer(w, r, make([]byte, 256<<10))
	return err
}
