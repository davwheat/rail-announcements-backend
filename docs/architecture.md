# Architecture

This document explains how the service is put together, why, and how the rest
of the website's announcement systems move into it.

## The pipeline

```
Darwin Browser ──protobuf──▶ feed.Hub ──▶ stream.Stream ──▶ queue.Queue
 /v1/announcements/live      one socket     one per zone      whose turn is it
                             per station          │
                                                  ▼
                       ketech.Voice.Announce ──▶ plan.Plan ──▶ audio.Library.Render ──▶ PCM
                       what to say               clips + pauses   decode, join            │
                                                                                          ▼
                                     listener ◀── HLS ◀── hls.Playlist  ◀── hls.Encoder (AAC)
                                     listener ◀── MP3 ◀── hls.Broadcast ◀── hls.Encoder (MP3, on demand)

POST /v1/announcements ──▶ system.System.Plan ──▶ plan.Plan ──▶ Render ──▶ MP3
```

Everything meets at `plan.Plan`: a list of clips, each with the silence that
leads into it. A voice produces a plan and the renderer plays one. Neither
knows about the other, and a plan is what the parity tests compare with the
website.

| Package | Role |
|---|---|
| `feed` | Darwin Browser's announcement stream: plain structs, protobuf decoding, and a hub that shares one upstream socket per station. |
| `plan` | Clips, pauses and the spoken-list helper. |
| `ketech` | The Amey/KeTech voices: what to say for a live announcement or for a posted tab state. |
| `queue` | Which announcement speaks next: duplicates, expiry, superseding, interruption. |
| `audio` | The clip library, the renderer and the MP3 encoder. |
| `hls` | The encoder processes, HLS segments and the playlist, and the broadcast behind the endless MP3 response. |
| `stream` | A listener's stream: queue, renderer and a real-time mixer that sums its zones. The manager starts and stops streams. |
| `system` | The registry of announcement systems behind `POST /v1/announcements`. |
| `api` | HTTP. |

## Decisions

### One stream for each listener, with the zones mixed in it

On the website, a *zone* is a group of platforms that share one playback lane.
Platforms in a zone take turns, and zones speak at the same time. The service
keeps that model exactly: a stream's queue has one lane for each zone, as the
website's queue does, and the mixer sums whatever the lanes are saying. A sum
that would overflow holds at full scale.

The first design was one stream for each zone, played side by side in the
browser. It was dropped because of background tabs. A browser throttles a
hidden tab's timers unless the tab is audible, and it judges that by output
level, so a station that is silent between announcements doesn't count. Several
script-fed players that all poll playlists are what throttling breaks. One
stream needs one player, and the browser can play it by itself.

The stream is the whole query string: station, zones with their platforms and
voices, announcement types and preferences. The service hashes the parsed query
into a key, and listeners with equal queries share one stream, one queue and
one encoder. Nothing is stored for a listener, so a URL is all a player needs,
and it keeps working after a restart. `platform=` still describes a stream of
one zone, for a listener who wants a zone by itself, such as a loudspeaker for
each zone.

### Two ways to take the stream

HLS can't be played by Chrome or Firefox without a script that fetches segments
and feeds them to the audio element, and that script is what a background tab
throttles. So the same mix is also served as one endless MP3 response, which
every browser plays from a plain `<audio>` element, as it plays internet radio.
The website uses HLS where the browser plays it natively, which is Safari, and
the MP3 response everywhere else.

- **MP3, not AAC.** AAC spends about 500 bytes a second on silence. A browser
  reads tens of kilobytes before it decides what it has been given, so on a
  quiet station it never started: this was tried first, and Chrome played
  nothing in 24 seconds. Constant bit rate MP3 sends silence as fast as speech.
- **A lead-in.** A player starts only once it holds a few seconds of audio,
  and it then stays that far behind, because audio that arrives in real time
  never lets it catch up. Each response opens with the last three seconds, which
  costs the same delay without the wait. Chrome then starts in about a second.
- **Started on demand.** The MP3 encoder is a second ffmpeg process, so a
  stream starts it when the first listener asks for `live.mp3`.
- **No live edge.** A stall leaves an MP3 player behind for good. The service
  closes a response that falls about six seconds behind, and the website's
  player skips forward when it holds more than four seconds of unplayed audio.

### Every stream hears the whole station

A stream's queue is given every announcement for the station, not only the ones
for its platforms. An announcement for platforms outside the stream occupies no
lane and finishes at once, so it never makes a zone wait. It still has to be
weighed, because it can change what the stream says: a platform alteration to a
platform outside the stream must cut short the announcement that is sending
people to the old platform.

### The queue is a synchronous state machine

The website's queue is built on promises and abort signals. The port has no
goroutines and no locks: the stream calls it from one goroutine, and hands
`Finished` back on that goroutine. That makes it deterministic, which is what
lets the parity test replay the website's traces step by step.

The website's five-minute playback timeout has no equivalent. Here a playback
is a block of rendered audio with a known length, so it can't stall. Rendering
is bounded by a one-minute timeout instead.

### PCM in the middle, ffmpeg at the edges

The recordings aren't uniform. Most are 16 kHz mono MP3, but others are 44.1
kHz, stereo, or at other bit rates, so MP3 frames can't be joined without
decoding them. The service decodes each clip to 44.1 kHz mono PCM, joins clips
in Go, and encodes once.

- **Same sound as the website.** A browser decodes these files with ffmpeg's
  decoders, the website joins them at 44.1 kHz, and it keeps only the first
  channel. The service does all three. A pause is the same number of samples as
  the website's `createSilence`.
- **ffmpeg does only codec work.** Go owns the timeline, the segments and the
  playlist, so they can be tested without ffmpeg. A stream runs one long-lived
  encoder process that reads PCM and writes ADTS frames.
- **Decoded clips are cached.** A station says the same few hundred words all
  day. The cache is bounded by `Audio.CacheMB`.

### Packed audio segments

A segment is raw ADTS frames behind an ID3 tag that holds the segment's
timestamp ([RFC 8216, section 3.4](https://datatracker.ietf.org/doc/html/rfc8216#section-3.4)).
No container is needed, so no muxer is either. hls.js in Chrome and ffmpeg both
play it, and both were checked. Safari's native player takes the same format
but hasn't been checked. The endless MP3 response was checked in Chrome.

Media sequence numbers start from the clock. A stream that stops when idle and
starts again keeps its URL, and a player that is still polling needs the
sequence to keep rising.

### Delay

A listener hears an announcement about two seconds after the service starts it,
on HLS with one-second segments and on the MP3 response alike. The website's
own audio has no such delay. Two
seconds is acceptable for every announcement except a fast train warning, where
it uses up some of the warning. `Stream.SegmentDuration` trades delay against
requests. If that isn't enough, the next step is Low-Latency HLS: partial
segments and blocking playlist reloads, which `hls.Playlist` can grow into
without changes elsewhere.

## Identical logic, and how that's kept true

The port is tested against the website's output and not against expectations
written by hand. The website's `npm run export:backend` runs the real
TypeScript and records what it does:

- **Live announcements.** 205 real movements captured from production, each
  turned into every kind of announcement, plus variations that real traffic
  rarely shows: odd platforms, delays, cancellations with every delay code,
  false destinations, request stops, cancelled calls, reversals and bus
  continuations. Both voices, three sets of preferences, about 2,700 cases.
  The test compares every clip, pause and error message.
- **Posted states.** Each tab's default state and presets, plus generated
  states for every way a train can divide, be short of a platform, or continue
  as a bus. About 800 cases.
- **Operator names and short platforms.** Table-driven.
- **The queue.** Scripted scenarios with the website queue's trace after each
  step.

The voices' tables (operators, platforms, delay codes, short platforms) are
exported as JSON and embedded, so they aren't ported by hand at all.

## Plan: every announcement system moves here

The website holds about 25 systems. Each one turns a tab's option state into a
list of clips and plays them in the browser. The goal is that the website posts
the state and plays what comes back:

```
POST /v1/announcements   {"system", "announcement", "state"}
  → 200 audio/mpeg
  → 4xx/5xx {"error": {"code", "message"}}
```

This endpoint exists, and the two KeTech voices answer it for all six of their
option tabs. The rest of the work repeats one loop for each system.

### For each system

1.  **Export.** Add the system to the website's `tests/backend-parity`
    generator. The capture is generic: it replaces `playAudioFiles`, runs a
    tab's play handler, and records the clips. Add the system's data tables to
    the export when it has any.
1.  **Port.** Write the system as a Go package that implements
    `system.System`, and register it in `system.All`. `Plan` receives the tab's
    state as JSON and returns a `plan.Plan`.
1.  **Prove.** Replay the exported cases until they pass. A system isn't
    switched over before that.
1.  **Switch.** Point the system's tabs at the service. See the next section.
1.  **Delete.** Remove the TypeScript handler. The Go package becomes the
    reference, and its exported cases become ordinary golden tests.

Suggested order: the remaining station systems first (ScotRail, Atos Anne, Atos
Matt), because they share the live announcement path and gain live streams at
the same time. Then the on-train systems, which are many but simple.

### What changes on the website

- `AnnouncementSystem` gains one method that posts `{system, announcement,
  state}` and plays the MP3 through `playRenderedAudio`, which already exists
  for audio that arrives rendered. A download saves the same bytes.
- A tab's `playHandler` becomes that method. The tab's options, presets and
  state don't change, and that's the point of posting the state as it is: the
  website's forms and saved presets keep working.
- A JSON error replaces today's `alert` calls. `missing_audio` names the
  recording, as `showAudioNotExistsError` does.
- The website stops needing the audio CDN for a system once it's switched.

### What the service still needs

- **Button tabs.** A `CustomButtonTab` plays a fixed list of clips. Export the
  lists with the system's data, and address a button as an announcement with
  the state `{"button": "LABEL"}`. The service doesn't accept raw clip lists
  from a client.
- **Per-system file naming.** Some systems override `processAudioFileId`, and
  train systems pick recordings by pitch. `System` gains a hook that maps a
  clip ID to a path when the first such system is ported.
- **Caching.** Identical states are common, because presets are. Cache rendered
  MP3s by a hash of the canonical request, and consider
  `GET /v1/announcements/HASH.mp3` behind a CDN if the traffic justifies it.
- **Limits.** The endpoint runs ffmpeg for each request. Add a concurrency
  limit and a per-client rate limit before it's public.
- **Live streams for other station systems.** `stream.Voices` looks voices up
  in `ketech` today. It becomes an interface with `Announce` and
  `AudioPlatform` when a second family of voices needs it.

### The other route to the listener

The feed's announcement stream can itself carry rendered audio: a connection
opts in with `audio=mp3`, and an announcement then arrives with its MP3. This
service's `ketech.Voice.Announce`, `audio.Library.Render` and `EncodeMP3` are
the three steps such a renderer needs. HLS was chosen for the website because
the listener's voices and zones are the listener's choice, which a shared
stream of announcements can't carry. Audio in the feed still suits a client
that wants one fixed voice and no player logic.
