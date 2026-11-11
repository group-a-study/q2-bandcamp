package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// File and folder names on the card (FAT): the characters it refuses replaced, no name ending in a
// dot or a space, and lengths kept so a song's whole path stays under 240 bytes, inside Q2 Pod's
// 256-byte path buffer (songs_open): /mnt/mmc/Bandcamp/ (18), an artist (50), an album (70) and a
// song (80, ".mp3" included).
//
//	Bandcamp/<artist>/<year> - <album>/07 - Title.mp3          an album, or a track sold alone
//	Bandcamp/Playlists/<playlist>/012 - Title.mp3              a playlist, kept as an album
//	Bandcamp/Radio/<date> - <series> - <show>/03 - Title.mp3   a radio show's songs, kept as an album

const (
	artistMax = 50
	albumMax  = 70
	songMax   = 76 // and ".mp3"
)

// cut shortens s to at most max bytes without splitting a character.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// safe is s as a FAT name of at most max bytes; unknown when nothing is left.
func safe(s string, max int, unknown string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`<>:"/\|?*`, r):
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	s = strings.Trim(cut(s, max), " .")
	if s == "" {
		return unknown
	}
	return s
}

// albumDir is a release's folder under Bandcamp/: its artist's, then the year first, so an
// artist's albums list oldest first wherever they are browsed by name.
func albumDir(artist, title string, date time.Time) string {
	a := safe(artist, artistMax, "Unknown Artist")
	if date.IsZero() {
		return filepath.Join(a, safe(title, albumMax, "Untitled"))
	}
	return filepath.Join(a, date.Format("2006")+" - "+safe(title, albumMax-7, "Untitled"))
}

// playlistDir is a playlist's folder under Bandcamp/.
func playlistDir(title string) string {
	return filepath.Join("Playlists", safe(title, albumMax, "Playlist"))
}

// showDir is a radio show's folder under Bandcamp/: its day first, so a series' shows list in order.
func showDir(series, title string, date time.Time) string {
	name := series
	if title != "" && !strings.EqualFold(title, series) {
		name += " - " + title
	}
	if !date.IsZero() {
		name = date.Format("2006-01-02") + " - " + name
	}
	return filepath.Join("Radio", safe(name, albumMax+20, "Show"))
}

// songFile is a song's file name: its number (three digits on a list of 100 or more) and title.
func songFile(num, of int, title string) string {
	width := 2
	if of >= 100 || num >= 100 {
		width = 3
	}
	prefix := ""
	if num > 0 {
		prefix = fmt.Sprintf("%0*d - ", width, num)
	}
	return prefix + safe(title, songMax-len(prefix), "Track") + ".mp3"
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// clock is seconds as m:ss or h:mm:ss.
func clock(s float64) string {
	n := int(s + 0.5)
	if n >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", n/3600, n/60%60, n%60)
	}
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

func joinNonEmpty(sep string, s ...string) string {
	var out []string
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}
