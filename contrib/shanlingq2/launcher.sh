#!/bin/sh
# Installed as /mnt/mmc/.bandcamp/run. Q2 Pod starts it from Streaming's Bandcamp row (a card app,
# q2-pod's patch/addon.c). It runs from /tmp, never the card: the app and this launcher are
# copied there first and the card's copy of this script is left at once (exec), so nothing keeps
# the card busy and USB storage can always take it. The loop restarts the app after a failure,
# waiting 5 s and doubling to a minute; an exit of 0 (stopped from the Q2, or idle) ends
# it. Q2 Pod stops it with a TERM to the loop's pid in /tmp/q2-bandcamp, the queue saved; an app
# still there 15 s after its TERM (frozen) is killed, so Stop always stops it. While the debug log is
# kept (q2-bandcamp.log at the card's root, debuglog.go), the launcher notes its starts, restarts and
# a failed copy there, and the app's stderr (a crash's message) and exit codes go to
# /tmp/q2-bandcamp.err, which the app's next start copies into the log.
pid=/tmp/q2-bandcamp
note() { echo "$(date '+%Y-%m-%d %H:%M:%S') launcher: $*" >> "$dir/../q2-bandcamp.log"; }
if [ "$1" != --loop ]; then
    dir=$(cd "$(dirname "$0")" && pwd)
    if [ -f $pid ] && kill -0 "$(cat $pid)" 2>/dev/null; then
        note "pressed; its loop runs already (pid $(cat $pid))"
        exit 0
    fi
    note "starting; /tmp: $(df -k /tmp | tail -1)"
    cp -f "$dir/q2-bandcamp" /tmp/q2-bandcamp.bin && chmod 755 /tmp/q2-bandcamp.bin ||
        { note "couldn't copy the app to /tmp"; exit 1; }
    cp -f "$0" /tmp/q2-bandcamp.run || { note "couldn't copy the launcher to /tmp"; exit 1; }
    cd /tmp
    exec /bin/sh /tmp/q2-bandcamp.run --loop "$dir"
fi
dir=$2
(
    child=
    stop() {
        if [ -n "$child" ] && kill -TERM $child 2>/dev/null; then
            (sleep 15; kill -KILL $child 2>/dev/null && note "the app was still there 15 s after its TERM; killed it") &
            k=$!
            wait $child
            kill $k 2>/dev/null
        fi
        rm -f $pid /tmp/q2-bandcamp.bin /tmp/q2-bandcamp.run /tmp/q2-bandcamp.err
        exit 0
    }
    trap stop TERM INT
    delay=5
    while :; do
        nice -n 10 /tmp/q2-bandcamp.bin -dir "$dir" 2>>/tmp/q2-bandcamp.err & # downloads: playback first
        child=$!
        wait $child
        code=$?
        [ $code = 0 ] && break
        echo "$(date '+%Y-%m-%d %H:%M:%S') launcher: the app exited with code $code; again in $delay s" >> /tmp/q2-bandcamp.err
        note "the app exited with code $code; again in $delay s"
        sleep $delay
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    note "the app ended (code 0)"
    rm -f $pid /tmp/q2-bandcamp.bin /tmp/q2-bandcamp.run /tmp/q2-bandcamp.err
) </dev/null >/dev/null 2>&1 &
echo $! > $pid
