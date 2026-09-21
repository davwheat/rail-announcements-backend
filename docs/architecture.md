# Architecture

This document explains how the service is put together, why, and how it serves
the website's announcement systems.

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

GET /v1/help-points/CRS ──▶ helppoint.Board ──▶ helppoint.Departures ──▶ plan.Plan ──▶ Render ──▶ MP3
                            Darwin Browser's     what to say
                            /v1/departures
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
| `system` | The registry behind `POST /v1/announcements`: the `System` interface, and `All`, which every port joins. |
| `systems/*` | One package for each system the website registers: a tab's option state in, a plan out. |
| `systems/shared` | What every port needs and none writes twice: button tabs as data, and the national station name table. |
| `systems/paritytest` | Replays a system's exported record against its port. |
| `helppoint` | The spoken departure board: Darwin Browser's board in, a plan in Phil's or Celia's voice out. |
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
- **A stall shorter than ten seconds costs no audio.** A player holds the three
  seconds the response opened with, and audio that arrives in real time never
  refills them, so a second the mixer skips is a second that listener loses for
  the rest of the response. A late tick writes everything it makes due in one
  block, up to `maxCatchUp`, which is ten seconds of audio, and warns that it
  was late. Only past that bound, where the clock jumped or the host was
  suspended, is audio dropped, and the mixer says how much.
- **Decoding gives way to the encoders.** The first announcement on a cold
  stream decodes dozens of clips, and on a host with few cores those ffmpeg
  processes compete with the encoders that have to keep real time. Decoding
  runs three clips at a time (`decodeWorkers`) at low priority (nice 10): an
  announcement that starts a moment later costs a listener nothing, and audio
  an encoder is too late to produce is gone.

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

The ports are tested against the website's output and not against expectations
written by hand. The website's `npm run export:backend` runs the real
TypeScript and records what it does:

- **Every system's tabs.** The export knows no one system: it replaces
  `playAudioFiles`, runs each tab's play handler over the tab's default state,
  its presets, every value of every dropdown, generated lists for custom
  options such as calling points, and seeded random mixes, and records the
  clips. About 41,000 cases over the 16 systems, replayed by each package's
  `parity_test.go` through `systems/paritytest`.
- **Live announcements.** 205 real movements captured from production, each
  turned into every kind of announcement, plus variations that real traffic
  rarely shows: odd platforms, delays, cancellations with every delay code,
  false destinations, request stops, cancelled calls, reversals and bus
  continuations. Both voices, three sets of preferences, about 2,700 cases.
  The test compares every clip, pause and error message.
- **The KeTech voices' posted states.** Each tab's default state and presets,
  plus generated states for every way a train can divide, be short of a
  platform, or continue as a bus. About 800 cases, on top of the generic
  export's.
- **Operator names and short platforms.** Table-driven.
- **The queue.** Scripted scenarios with the website queue's trace after each
  step.

A recorded plan wins over a recorded alert. A case holds both what the handler
played and what it alerted, and a handler that alerts and plays anyway hasn't
refused the state: the website shows that alert in the browser itself and asks
this service only for the audio. So the port has to build the plan, and only a
case that played nothing expects a refusal, with the same message.

A system's data is exported as JSON and embedded, so it isn't ported by hand at
all: the voices' tables (operators, platforms, delay codes, short platforms),
each system's own fields and module constants, and the button tabs.

A record proves the clip names, and nothing more. `system`'s render test then
plays a sample of every system's recorded cases through the audio library,
which is what catches a wrong file prefix or a path rule that names nothing.

## Every announcement system is served here

Each of the website's systems turns a tab's option state into a list of clips
and plays them in the browser. Every system it registers is ported, so the
website can post the state and play what comes back instead:

```
POST /v1/announcements   {"system", "announcement", "state"}
  → 200 audio/mpeg
  → 4xx/5xx {"error": {"code", "message"}}
```

A port lives in `internal/systems`, one package for each system, and joins
`system.All`. It ports the play handlers and nothing else: the system's own
fields and module constants arrive from the website as JSON, and a button tab
is data too, so a port answers one by looking the button up. The tab's options,
presets and state are untouched, which is the point of posting the state as it
is — the website's forms and saved presets keep working, and `GET /v1/systems`
tells the page which tabs to post.

### How the website asks

**Announcement audio** is one setting for the whole website, held in
`serviceAudioState` and shown in the footer. With it on, a pane sets
`AnnouncementSystem.serviceRequest` to the tab and its state around the tab's
own play handler, and `playAudioFiles` asks this service for the audio in place
of joining the clips it was given. A download saves the same bytes as an MP3,
where the website's own is a WAV.

The handler still runs, and that's deliberate: it's what refuses a state that
can't be announced, and what keeps the Piccadilly line's on-page display in
step with the audio. Button tabs go through the same path, from a state of
`{"section", "label"}`. If this service can't be reached, or refuses the state,
the website falls back to the handler's clips, so the setting never costs a
listener the announcement. A JSON error stands in for the website's own
`alert`, and `missing_audio` names the recording, as `showAudioNotExistsError`
does.

### What's left on the website

- **The TypeScript is still the reference.** The website holds both
  implementations, and its export is what the ports are tested against. A
  system's Go package becomes the reference only once the TypeScript play
  handler is deleted, and its exported cases then become ordinary golden tests.
- **Three systems aren't registered, and aren't ported**: `WMTClass172`,
  `WMTClass323` and `AvantiPendolino`. Registering one in `AllSystems.ts`
  brings it into the export, and from there it's an ordinary port.

### What the service still needs

- **Caching.** Identical states are common, because presets are. Cache rendered
  MP3s by a hash of the canonical request, and consider serving them from
  `GET /v1/announcements/HASH.mp3`, where `HASH` is that hash, behind a CDN if
  the traffic justifies it.
- **Limits.** The endpoint runs ffmpeg for each request. Add a concurrency
  limit and a per-client rate limit before it's public.
- **Live streams beyond the KeTech voices.** A stream is built by
  `stream.Voices`, which looks a voice up in `ketech`, so only those two voices
  have one. It becomes an interface with `Announce` and `AudioPlatform` when a
  second family of station voices needs a stream; a posted state needs none of
  that.

### The other route to the listener

The feed's announcement stream can itself carry rendered audio: a connection
opts in with `audio=mp3`, and an announcement then arrives with its MP3. This
service's `ketech.Voice.Announce`, `audio.Library.Render` and `EncodeMP3` are
the three steps such a renderer needs. HLS was chosen for the website because
the listener's voices and zones are the listener's choice, which a shared
stream of announcements can't carry. Audio in the feed still suits a client
that wants one fixed voice and no player logic.

## The help point

`GET /v1/help-points/CRS` speaks a station's departure board, the way the
button on a platform help point does. `helppoint.Board` reads the next 90
minutes of passenger trains, to a limit of eight, from Darwin Browser's
`/v1/departures`, and `helppoint.Departures` turns them into a plan in the
voice that `?voice=` names: `phil`, which is the default, or `celia`. The
renderer and the MP3 encoder are the ones a posted state uses.

The package isn't a port. The website's **Help point** tab plays this endpoint
and builds nothing itself, so there is no record to compare with, and the
wording is decided here. It borrows two things from `ketech`: the voice's file
prefix, and its copy of the website's table of delay codes.

Four decisions shape it:

-   **A missing recording is left out.** The plan's missing audio mode is
    `play-silence`. A board names stations that Phil never recorded, and a
    listener is better served by a board with one gap than by no board. Where
    a gap would mislead, the package asks the library first: a reason is spoken
    only when every clip of it exists, so "due to" is never left alone.
-   **A combined recording is preferred.** Phil recorded "the next service from
    platform 4 will be the" and "Southern service to" as single clips for most
    platforms and operators. The package asks `audio.Library.Exists` for the
    combined clip, and joins the parts when there is none.
-   **The wording is Phil's, and Celia says it with the clips she has.** She
    never recorded "service" or "this service" in the inflections the board
    uses, nor "hour" and "hours" at the end of a sentence.
    `announcement.recorded` takes the clip in another inflection, or "this
    train" for "this service", when the voice lacks the first choice. A test
    plans a board that takes every turn of the wording, in both voices, and
    fails on a clip that has no recording.
-   **A failure is spoken.** When the board can't be read, the response is
    still `200` and an MP3: "We regret that the information facility is not in
    operation." What plays the response has a listener and no screen, and a
    browser's `<audio>` element plays nothing for an error status. Only a
    request that is itself wrong gets a JSON error.

The endpoint holds nothing between requests, so the proxy sends it to either
replica.
