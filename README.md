# q2-bandcamp

Bandcamp for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod): browse the editors' **New and Notable** picks, Bandcamp Daily's **Album of the Day**, featured and fans' **playlists** and every **Bandcamp Radio** show on the Q2, and download what you pick to the card as [bandcamp-dl](https://github.com/evolution0/bandcamp-dl) does: the 128 kbps MP3s Bandcamp's own player streams, tagged, with the cover and lyrics. It then plays from the card as local music, with no Wi-Fi. No Bandcamp account is needed. It needs a Q2 Pod build that supports card apps.

These are the same streams anyone hears on bandcamp.com for free. If you like an album, buy it from the artist on Bandcamp: that's how they get paid, and you get it in full quality.

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **Bandcamp** (once Q2 Pod's app list carries it).

**By hand:**

1. Download `q2-bandcamp-*.zip` from the [latest release](https://github.com/group-a-study/q2-bandcamp/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.bandcamp/q2-bandcamp`, `.bandcamp/run`, `.bandcamp/q2app`, `.bandcamp/icon.png` and `.bandcamp/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

Bandcamp then has a row in **Streaming**, with its icon. The Q2 needs Wi-Fi.

## Use

Bandcamp's page has **New and Notable**, **Album of the Day**, **Playlists**, **Bandcamp Radio**, **Search Bandcamp**, **Downloads** (while there are any), **Downloaded** and **Stop Bandcamp**.

- **New and Notable**: the 120 latest picks of Bandcamp's editors, newest first, with each album's cover, artist and genre. Its first row, **Download the Top 10**, downloads the 10 newest picks. **How Many** sets that number: 1, 3, 5, 10 (the first choice), 15, 20, 25, 30, 40, 50, 75, 100 or all 120.
- **Album of the Day**: Bandcamp Daily's 120 latest, with the day each was picked. **Download the Latest 10** downloads the most recent ones, as many as its own **How Many** says.
- **Download the Top N passes over every album you downloaded before**: the ones on the card, and the ones you deleted since. So pressing it again only fetches picks that are new.
- **Press an album** to download it. One on the card turns into a row of the card: a press lists its songs and plays them from the card. One downloading says how far it has got; one you deleted says so, and a press downloads it again.
- **Delete an album from the card**: hold **Play/Pause** on its row twice (within 5 seconds), or open it in **Downloaded** and press **Delete from the Card** twice. Its songs and cover are removed, and its folder too when nothing else is in it; Q2 Pod then updates its library. It stays in the record.
- **Downloaded** is the record of everything downloaded, the newest first, on the card or deleted since. Open one for its songs, **Delete from the Card**, or **Download Again** once it's deleted. The record is also on the card as `Bandcamp/Downloaded.txt` (below).
- **Playlists**: the playlists Bandcamp features; a fan's: search for the fan's name. A playlist's page has **Download Playlist** and its songs: a press on a song downloads the album it's from.
- **Bandcamp Radio**: every show, in its series (Bandcamp Weekly, Bandcamp Selects, The Metal Show, The Hip Hop Show, Bandcamp Electronic, The Indie Show, …), the newest first; **Latest Shows** has them all. A show's page has **Download the Songs** and its songs, each with the minute it played at: a press on one downloads the album it's from.
- **Search Bandcamp** finds albums (press to download), artists and labels (their page lists every release, with **Download All** first, as bandcamp-dl's `--artist`, passing over what you downloaded before) and fans (their playlists).
- **Downloads** shows the album downloading with its bar, the ones waiting (a press removes one), and what finished: a press plays it; one that failed, a press tries it again.

Lists too long for one page end in **Next Page** (and start with **Previous Page**).

Downloads carry on with the page closed, one song at a time, and wait by themselves while the Q2 is offline or the card is away; the Q2 stays awake while a song downloads. A dropped connection resumes where it stopped. A song that fails for its own reason is tried 3 more times, then left out and named in Downloads, where a press tries the album again (only what's missing is fetched). A song Bandcamp doesn't stream (sold only, or not out yet) is left out and counted. When the downloads are done, Q2 Pod runs **Update Local Music** by itself once nothing is playing (Q2 Pod 1.7 or later), so new albums join Artists, Albums and Coverflow.

Bandcamp stops by itself 15 minutes after its page was last used with nothing to download, which lets the Q2 turn its Wi-Fi off. **Stop Bandcamp** stops it at once; what's left to download goes on at its next start.

## Where the music goes

A release goes in `Bandcamp/`, by its artist, then its year and title, its songs numbered:

    Bandcamp/Dawuna/2026 - Love's Arp/02 - Love's Ekongo.mp3
    Bandcamp/Dawuna/2026 - Love's Arp/cover.jpg

Each song is tagged with its title, artist, album artist, album, track number, year, genre, label, its Bandcamp page, its lyrics when Bandcamp has them, and the 700 px cover; `cover.jpg` is beside them.

**A playlist and a radio show are kept as albums**, as q2-applemusic keeps playlists:

    Bandcamp/Playlists/ambient reading/001 - In Åland Air.mp3
    Bandcamp/Radio/2026-11-09 - Bandcamp Selects - Robocobra Quartet/02 - AAAH!.mp3

Their songs are tagged with the playlist's name (or the show's, "Bandcamp Selects: Robocobra Quartet") as their album, **Playlists** or **Bandcamp Radio** as their album artist, as a compilation, numbered in the list's order. Each song keeps its own album's cover; the list's picture is `cover.jpg`, and a hidden `.q2-playlist` file tells Q2 Pod to show it for the album. **Download Again** on a list's page fetches the songs not on the card yet; **Delete from the Card** there deletes it.

## The record

`Bandcamp/Downloaded.txt` lists everything Bandcamp downloaded, oldest first, tab-separated (a spreadsheet opens it): kind (album, playlist, radio show), title, artist, songs, when it was downloaded, when it was deleted, its folder and its Bandcamp page. It's rewritten whenever something finishes or is deleted. An album deleted by hand (File Browser, a computer) is noticed and marked deleted too, the next time Bandcamp starts or **Downloaded** is opened. The record itself is kept in `.bandcamp/done.json`; deleting `Downloaded.txt` loses nothing (it's written again), deleting `done.json` forgets the record.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace the five files in `.bandcamp`. Either way, `.bandcamp/queue.json` (what's left to download), `.bandcamp/done.json` (the record), `.bandcamp/settings.json` (How Many) and `.bandcamp/partial` (a song half downloaded) are kept.

To remove it, press **Bandcamp** twice in **System settings → Apps**, or delete `.bandcamp`. Your music in `Bandcamp/` stays.

## The log

Bandcamp keeps a log at the card's root, `q2-bandcamp.log` (the one before it as `q2-bandcamp.old.log`, half a megabyte each): its starts and stops, the lists it fetched and how long they took, each download, failure and deletion, and the memory. If something goes wrong, the log says what Bandcamp saw. File Browser shows it.

## How it works

Bandcamp has no public API; the app asks bandcamp.com what its own pages ask, and says what it is in every request (as bandcamp-dl does). See [contrib/shanlingq2/README](contrib/shanlingq2/README) for the Q2's side and `internal/bandcamp` for Bandcamp's:

- New and Notable and Album of the Day: `editorial/3/get_new_and_notable` and `get_aotd`.
- Playlists: `playlist/1/featured_playlists`, a fan's `fan_collection/1/playlists`.
- Bandcamp Radio: `radio_api/1/get_radio_shows` (with `bcweekly/3/list` naming the older shows' series).
- A playlist's or a show's songs: `player/2/player_data_web`.
- A release to download: its own page, read as bandcamp-dl reads it (`data-tralbum`: each track's `mp3-128` stream, number, artist and lyrics; `ld+json`: tags and label).
- Search: `bcsearch_public_api/1/autocomplete_elastic`; an artist's releases: their `/music` page.

## Build

    contrib/shanlingq2/build.sh

Go 1.22 or later and python3; it has no dependencies outside Go's standard library. `go test ./...` runs the tests against a fake Bandcamp; `Q2_BANDCAMP_LIVE=1 go test -run Live ./internal/bandcamp` and `Q2_BANDCAMP_LIVE=<an empty folder> go test -run LiveDaemon -v ./cmd/q2-bandcamp` ask the real one (the second downloads an album and a radio show's songs into the folder, then deletes the album).

## Licence

MIT. The download follows bandcamp-dl, which is in the public domain (The Unlicense). The Mozilla CA bundle in `internal/tlsroots` is under the MPL-2.0.
