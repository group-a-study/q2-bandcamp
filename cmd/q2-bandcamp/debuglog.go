package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-bandcamp/internal/q2"
)

// The debug log, which the user allowed for this app (2026-11-11): q2-bandcamp.log at the card's
// root, where File Browser shows it. A line each for a start (and what the run before it left in
// /tmp: a crash's message, the launcher's restarts and exit codes), each page Bandcamp was asked
// for and how long it took, each job and song, failures and waits, streams started and proxied,
// memory every minute, and why it stopped. Each line is appended and synced, the file opened and
// closed around it, so it never keeps the card busy; past logMax it starts again, the last one kept
// as q2-bandcamp.old.log. The launcher (contrib/shanlingq2/launcher.sh) writes its own lines there.

// logPath is the log's file, "" for none (tests).
var logPath = ""

const logMax = 512 << 10

var (
	logMu   sync.Mutex
	started = time.Now()
)

func logf(format string, args ...any) {
	if logPath == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + fmt.Sprintf(format, args...) + "\n"
	logMu.Lock()
	defer logMu.Unlock()
	if !q2.CardMounted() {
		return
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size()+int64(len(line)) > logMax {
		os.Rename(logPath, strings.TrimSuffix(logPath, ".log")+".old.log")
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line)
	f.Sync()
	f.Close()
}

// errFile is where the launcher puts the app's stderr (a crash's message) and its restarts.
const errFile = "/tmp/q2-bandcamp.err"

// free is a file system's free space, in MB.
func free(path string) string {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return "?"
	}
	return fmt.Sprintf("%d MB", uint64(st.Bavail)*uint64(st.Bsize)>>20)
}

// memAvailable is the system's MemAvailable and MemFree, from /proc/meminfo.
func memAvailable() string {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return "?"
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "MemAvailable:") || strings.HasPrefix(l, "MemFree:") {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
	}
	return strings.Join(out, ", ")
}

// logStart notes a start: what the run before it left, the card's and /tmp's free space, the queue.
func logStart(d *daemon) {
	if b, err := os.ReadFile(errFile); err == nil && len(b) > 0 {
		if len(b) > 8<<10 {
			b = b[len(b)-8<<10:]
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			logf("before this start: %s", l)
		}
		os.Truncate(errFile, 0)
	}
	d.dl.mu.Lock()
	queued, finished := len(d.dl.queue), len(d.dl.done)
	d.dl.mu.Unlock()
	logf("start %s (pid %d): card free %s, /tmp free %s, %s; %d jobs queued, %d finished; caps %q; proxy port %d",
		version, os.Getpid(), free(q2.Card), free("/tmp"), memAvailable(), queued, finished, caps(), d.st.port)
}

// caps is the firmware's card-app features, as it lists them.
func caps() string {
	b, _ := os.ReadFile(q2.CapsFile)
	return strings.TrimSpace(string(b))
}

// logMemory notes the memory, the system's and the app's, and /tmp.
func logMemory(d *daemon) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	d.dl.mu.Lock()
	queued := len(d.dl.queue)
	d.dl.mu.Unlock()
	logf("memory: app heap %d MB, app system %d MB, %d goroutines; %s; /tmp free %s; %d jobs queued; up %v",
		m.HeapAlloc>>20, m.Sys>>20, runtime.NumGoroutine(), memAvailable(), free("/tmp"), queued,
		time.Since(started).Round(time.Second))
}
