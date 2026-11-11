package id3

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// frames reads a v2.4 tag back: each frame's body by its id.
func frames(t *testing.T, b []byte) (map[string][]byte, int) {
	t.Helper()
	if string(b[:3]) != "ID3" || b[3] != 4 {
		t.Fatalf("not a v2.4 tag: % x", b[:10])
	}
	size := syncsafe(b[6:10])
	body := b[10 : 10+size]
	out := map[string][]byte{}
	for len(body) >= 10 && body[0] != 0 {
		n := syncsafe(body[4:8])
		out[string(body[:4])] = body[10 : 10+n]
		body = body[10+n:]
	}
	if len(bytes.Trim(body, "\x00")) != 0 {
		t.Fatalf("junk after the frames")
	}
	return out, 10 + size
}

func TestBuild(t *testing.T) {
	cover := []byte{0xff, 0xd8, 1, 2, 3}
	tag := Tags{Title: "Love’s Ekongo", Artist: "Dawuna", AlbumArtist: "Dawuna", Album: "Love's Arp", Track: 2, Tracks: 13,
		Year: "2026", Genre: "R&B/Soul", Label: "True Panther", URL: "https://dawuna.bandcamp.com/album/loves-arp",
		Lyrics: "line one\nline two", Compilation: true, Cover: cover}.Build()
	f, n := frames(t, tag)
	if n != len(tag) {
		t.Fatalf("size %d, tag %d", n, len(tag))
	}
	want := map[string]string{"TIT2": "\x03Love’s Ekongo", "TPE1": "\x03Dawuna", "TPE2": "\x03Dawuna", "TALB": "\x03Love's Arp",
		"TRCK": "\x032/13", "TDRC": "\x032026", "TCON": "\x03R&B/Soul", "TPUB": "\x03True Panther", "TCMP": "\x031",
		"WOAF": "https://dawuna.bandcamp.com/album/loves-arp", "USLT": "\x03eng\x00line one\nline two",
		"APIC": "\x00image/jpeg\x00\x03\x00" + string(cover)}
	for id, v := range want {
		if string(f[id]) != v {
			t.Errorf("%s = %q, want %q", id, f[id], v)
		}
	}
	if len(f) != len(want) {
		t.Errorf("%d frames, want %d", len(f), len(want))
	}
	// left out when empty
	f, _ = frames(t, Tags{Title: "x"}.Build())
	if len(f) != 1 {
		t.Errorf("empty fields made frames: %v", f)
	}
}

func TestWriteReplacesTags(t *testing.T) {
	audio := []byte{0xff, 0xfb, 0x90, 0x64, 'I', 'n', 'f', 'o'}
	old := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 4, 'J', 'U', 'N', 'K'}
	var out bytes.Buffer
	if err := Write(&out, bytes.NewReader(append(old, audio...)), Tags{Title: "T"}); err != nil {
		t.Fatal(err)
	}
	_, n := frames(t, out.Bytes())
	if !bytes.Equal(out.Bytes()[n:], audio) {
		t.Fatalf("audio after the tag: % x", out.Bytes()[n:])
	}
	if binary.BigEndian.Uint16(out.Bytes()[n:]) != 0xfffb {
		t.Fatal("the MPEG frame isn't first after the tag")
	}
}
