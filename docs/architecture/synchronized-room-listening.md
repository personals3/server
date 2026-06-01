# Synchronized Room Listening — Architecture

> Design notes for the "x people in a room hear the same beat at the same wall-clock instant" feature. PersonalS3 hosts the bytes; the room app handles sync. This document is for the room app — PersonalS3 itself doesn't need to change.

## The two problems are independent

Streaming format and inter-client synchronization are orthogonal. Solving one does not solve the other.

| Concern | Owned by |
|---|---|
| Getting the audio bytes to each client | The streaming protocol (HLS / MP3 / WebRTC) |
| Making sure all clients render the same sample at the same wall-clock millisecond | A sync layer **on top** of the streaming |

Two clients can both be playing the same HLS stream and end up 3 seconds apart, depending on when each pressed play, network conditions, and decoder buffering. HLS does not synchronize them. **Synchronization is application-level state plus client-side scheduling.**

This is why we don't need to change PersonalS3's transcode pipeline — the bytes are already produced correctly. The room app builds the sync layer on top.

## What "low latency between users" actually means

The goal: any two listeners in the same room should be playing the same audio sample within **±20 ms** of each other (the user-perceived threshold for "out of sync"). Within ±10 ms is unnoticeable. Within ±50 ms is "noticeable but acceptable for most music". Below ±5 ms is studio-grade and rarely needed.

Realistic latency budgets in practice:

| Architecture | Inter-client jitter |
|---|---|
| Two `<audio>` elements + "play now" broadcast | ±200–500 ms |
| WebSocket sync + Web Audio API scheduled start | ±10–30 ms |
| WebSocket sync + Web Audio API + drift correction loop | ±5–15 ms |
| WebRTC sub-stream (a media server forwards a single stream) | ±2–5 ms |

The middle two rows are what mainstream watch-party apps (Spotify Group Session, JQBX, Watch2Gether) achieve. They're the right target for this feature — ambitious but tractable, well-understood patterns, no exotic infrastructure.

## Architecture summary

```
┌──────────────────┐
│ Room state      │  Postgres or Redis. Source of truth for what's
│  (server)       │  playing and when it started in server time.
└──────────────────┘
        │
        │ WebSocket fan-out
        ▼
┌─────────────────────────────────────────────────────────────────┐
│ Client (browser)                                               │
│                                                                 │
│  1. Open WebSocket to /room/{id}                                │
│  2. Receive { trackId, startedAtServerMs }                      │
│  3. Measure server-clock offset (NTP-style ping)                │
│  4. Fetch /stream/<id>/audio_320.mp3                            │
│  5. decodeAudioData() → AudioBuffer (one-shot CPU spike)        │
│  6. const playOffset = (now_server - startedAtServerMs) / 1000  │
│  7. source.start(audioContext.currentTime, playOffset)          │
│  8. Every 30 s: re-measure offset, nudge source.playbackRate    │
│     by ±0.0005 to correct drift without audible pitch shift     │
└─────────────────────────────────────────────────────────────────┘
```

Five distinct mechanisms:

1. **Room state.** Server holds `{ track_id, started_at_server_ms, host_user_id, paused }`. Mutations go through the host or through a "DJ rotation" rule the app decides on.
2. **Pub/sub.** WebSocket (or SSE) so every client receives state changes within ~50 ms. Includes joins, leaves, track changes, pause/resume.
3. **Clock sync.** An NTP-style endpoint clients hit periodically to learn their offset to server time. Five samples gives ±5 ms accuracy on a typical internet connection.
4. **Audio scheduling.** Web Audio API — NOT the `<audio>` element. The element schedules "play when buffered"; the API schedules "play *exactly* at time T". Sample-accurate.
5. **Drift correction.** Sound cards have clock drift (~50 ppm = 50 ms per 17 minutes). Every 30 s, re-measure offset, compute the residual, micro-adjust `source.playbackRate` to converge.

## Why the `<audio>` element won't get you there

`<audio src="...mp3">` is convenient but it has no "play at wall-clock time T" API. It plays "as soon as buffered". On a fresh page load:

- Buffer fills in 50–500 ms depending on connection and prefetch heuristics
- Browsers throttle background tabs (some pause buffering entirely)
- There's no way to query or set the audio output clock offset

Two users on the same page can be **±300 ms apart** even after both click play simultaneously. For room-listening, that's clearly out of sync.

`<audio>` is fine for *individual* progressive playback (it does stream MP3 via Range, not download the whole file — see "MP3 streams progressively" below). It's just not the right primitive for *synchronized* playback.

## Web Audio API as the playback primitive

The Web Audio API lets the browser schedule audio at a precise time on its high-resolution clock:

```
audioContext.currentTime              // monotonic seconds since context create
source.start(when, offset, duration)  // schedule a buffer to play at `when`
source.playbackRate.value             // 1.0 = normal; 1.0005 = +0.05% faster
```

Sample accuracy at 44.1 kHz = **22 μs** precision. The challenge isn't browser support (it's universal) — it's mapping `audioContext.currentTime` to server wall-clock time, which is what the sync layer below does.

## Clock synchronization (NTP-style)

The client doesn't trust its own wall-clock (`Date.now()`) for cross-client coordination — different machines have different system clocks. Instead it learns its offset to the server clock:

```
        Client                Server
t0  ──► GET /sync/clock ──► t1
                              wait
t3  ◄── { t1, t2 }       ◄── t2

round_trip = (t3 - t0) - (t2 - t1)
one_way    = round_trip / 2
offset     = ((t1 - t0) + (t2 - t3)) / 2
```

Run 5 samples back-to-back, discard the slowest one (network spike), median the rest. Yields ±3–10 ms accuracy on the internet, ±0.5–2 ms on a LAN. Re-run every 30 seconds to track drift.

Server endpoint is dead simple:

```
GET /sync/clock → { ts: <server_unix_ms_now> }
```

Two timestamps in the response (`t1`, `t2`) only matter if the server takes significant time to process the request — usually they're the same.

## Putting clock sync and Web Audio together

```
serverTimeNow = Date.now() + clockOffset   // derived
elapsed_ms    = serverTimeNow - startedAtServerMs
playOffset_s  = elapsed_ms / 1000

if (playOffset_s < 0) {
  // Track hasn't started yet (rare — only for scheduled-future starts).
  // Schedule the start time:
  source.start(audioContext.currentTime + (-playOffset_s), 0)
} else if (playOffset_s < trackDuration) {
  // Mid-track join: jump to that offset.
  source.start(audioContext.currentTime, playOffset_s)
} else {
  // Track ended; wait for next state update.
}
```

`audioContext.currentTime` is the browser's monotonic audio clock — totally independent of `Date.now()` and not affected by user clock changes or NTP corrections to the OS clock. That isolation is exactly what makes Web Audio sample-accurate.

## Drift correction

Even after a perfect initial start, audio cards drift. Cheap consumer hardware has 50–100 ppm clock error, meaning ~50–100 ms drift per 17 minutes of playback. Without correction, room-mates fall out of sync mid-album.

The pattern every 30 seconds:

1. Re-measure server offset (one NTP exchange)
2. Compute "where should I be now?" → `target_offset = (now_server - startedAtServerMs) / 1000`
3. Compute "where AM I?" → `actual_offset = audioContext.currentTime - sourceStartedAt + sourceStartOffset`
4. Compute residual → `error = target_offset - actual_offset`
5. If `|error| > 50 ms`: too far out, do a hard re-sync (stop, restart at the target offset). Audible blip — should be rare.
6. If `|error| < 50 ms`: micro-adjust playback rate. Set `playbackRate = 1.0 + (error * 0.01)` for a few seconds, then back to 1.0. Inaudible.

This second-derivative correction (rate adjustment, not position adjustment) is what keeps room-listening tools imperceptibly synced over hours.

## Late joiners

Same path as initial join. The client receives the current state, measures clock offset, computes the play offset, calls `source.start(when, offset)`. Zero special-casing needed.

The user perceives:

1. Click "join room" (joining takes a moment, network round-trips)
2. Audio fetched in parallel with state messages — done in ~500 ms for a small MP3
3. Decoded into `AudioBuffer` — ~100 ms CPU spike
4. First sample plays exactly aligned with everyone else

Total perceived "join to playing" latency: ~500–1000 ms for the first time, faster on subsequent joins because the file is cached.

## Network protocol sketch

WebSocket message types from server to client:

```
{ "type": "state",       "trackId": "...", "startedAtServerMs": 1234567890123,
                          "trackDurationMs": 213000, "paused": false }
{ "type": "trackChange", "trackId": "...", "startedAtServerMs": ... }
{ "type": "pause",       "atServerMs": ... }      // playhead stops here
{ "type": "resume",      "atServerMs": ..., "fromOffsetMs": ... }
{ "type": "userJoined",  "userId": "...", "name": "..." }
{ "type": "userLeft",    "userId": "..." }
{ "type": "chat",        "userId": "...", "text": "..." }
```

Client to server:

```
{ "type": "queueTrack",  "trackId": "...", "afterTrackId": "..." }  // host only
{ "type": "skipNow" }                                                // host only
{ "type": "pause" }       /  { "type": "resume" }                    // host only
{ "type": "reportLatency", "measuredMs": 12 }                        // optional telemetry
```

Host-only operations need a separate rule for "who is the host" — fixed creator, round-robin, vote, etc. Application layer's call.

## What PersonalS3 needs to provide

Surprisingly little. The audio bytes endpoint already covers it:

```
GET /stream/<object_id>/audio_320.mp3
```

That's it. The MP3 is the right format for this use case — it decodes to a single `AudioBuffer` on the client and gets scheduled precisely. HLS adds complexity (segment management, decoder restart on segment boundaries) without benefit for tight-sync playback.

If the audio file is large (>50 MB / >30 minutes), consider also offering:

```
GET /stream/<object_id>/audio.m3u8
```

The client could partially decode HLS segments into a rolling `AudioBuffer` queue — saves memory at the cost of more complex scheduling. For most consumer music (3–6 minute songs, 5–15 MB), one-shot fetch is simpler and fine.

## Comparison with alternative approaches

### "Just broadcast the audio with WebRTC"

A single sender encodes the audio and forwards it to all listeners via WebRTC (SFU model). All listeners receive the same stream; sync is implicit.

- **Pros:** Lowest possible latency (~50 ms end-to-end). True real-time (someone can sing along live).
- **Cons:** Requires a media server (Janus, mediasoup, livekit). Re-encoding costs CPU. New users can't seek back. Late joiners only hear from-now-forward. Not appropriate for "listen to library track together" use cases.
- **When right:** Live DJ sets, voice chat over music, jam sessions.

### "Server sends signed playback timestamps"

Same as the Web Audio approach but the server pushes ticks every second instead of the client polling clock offset.

- **Pros:** Less client logic. Lower overhead.
- **Cons:** Server has to maintain per-client tick streams. Clock offset accuracy is bounded by message delivery jitter.
- **When right:** Tightly controlled networks (in-venue, single LAN).

### "Use the host's audio output as the source of truth"

Host plays via `<audio>`, broadcasts its current `currentTime` every second. Others nudge their own playback to match.

- **Pros:** Trivial to implement. Works without Web Audio API.
- **Cons:** Per-client drift accumulates between updates. ±200 ms is the realistic floor. Host disconnecting breaks everyone.
- **When right:** Casual rooms where rough sync is "good enough". Not for music.

The clock-sync + Web Audio approach is the **standard** for this problem and the right default.

## Implementation order

1. **Add a `/sync/clock` endpoint** to the room app. One-liner: return `Date.now()`.
2. **Add a WebSocket endpoint** for room state. Fan out `state` messages on join, pause, resume, track change.
3. **Client-side `RoomSync` class** that:
   - Opens the WebSocket
   - Measures clock offset every 30 s
   - Fetches the MP3 once per track and decodes into `AudioBuffer`
   - Schedules playback at the right offset
   - Runs the drift-correction loop
4. **Telemetry: log the measured offset and drift per client to a `room_sync_events` table.** Useful for tuning later.
5. **Stretch: feature flag for HLS-mode playback** — same architecture, just swap `<audio_320.mp3>` fetch + `decodeAudioData()` with HLS segment fetching. Worth it only after MP3-mode is solid.

## Open questions for the design

- **Who's the host?** Fixed creator, round-robin, vote, anyone-with-the-pause-power? App's choice.
- **What happens on network blip?** Client should keep playing through small interruptions (jitter buffer ahead of playhead), re-sync on reconnect.
- **Pause-resume semantics:** the state needs a "current_offset_ms" field that gets frozen on pause and resumed from on play. Otherwise you can't pause without losing position.
- **Track end:** auto-advance to next track in queue? Stay on the last frame? App's choice.
- **Range limits:** seek is just a state update with a new `startedAtServerMs` derived from `(now - desired_offset)`. Easy primitive once the sync layer exists.

## Glossary

- **Wall-clock time:** what `Date.now()` returns, modulo OS clock corrections.
- **Audio clock:** what `AudioContext.currentTime` returns — monotonic, independent of OS clock, sample-accurate.
- **Clock offset:** `audioOrServerTime - localTime` — the constant we add to local time to get the shared reference.
- **Drift:** the rate at which two clocks diverge. Measured in ppm (parts per million).
- **Jitter:** variance in network message delivery time. Different from drift; cured by buffering, not by rate adjustment.

## References for further reading

- W3C Web Audio API spec — `AudioContext.currentTime`, `AudioBufferSourceNode.start()`
- RFC 5905 — NTPv4 protocol; we use a simplified variant
- "Trade-offs in audio synchronization" — surveys of media-room implementations
- mediasoup / livekit docs — if the WebRTC alternative becomes interesting later
