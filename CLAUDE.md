# CLAUDE.md

This service speaks station announcements: it reads Darwin Browser's protobuf
announcement stream, builds announcements from the website's recordings, and
serves them as one mixed stream for each listener, as HLS
(`GET /v1/streams/live.m3u8`) or as an endless MP3 response (`live.mp3`). It
also renders one MP3 for a posted tab state (`POST /v1/announcements`). Read `README.md` for the API and
`docs/architecture.md` for the design, the decisions and the migration plan.

## Rules that aren't obvious

- **The website is the reference for announcement logic.** `internal/ketech`
  and `internal/queue` are ports of the website's TypeScript
  (https://github.com/davwheat/rail-announcements). Don't change their
  behavior here first: change the TypeScript, run `npm run export:backend` in
  the website (not `yarn`: that repository forbids it), and port until
  `go test ./...` passes. The quirks are deliberate. For example, Celia's named
  Great Western services are matched case-sensitively, and a short platform's
  wording uses the end inflection where the middle one looks right.
- **Don't edit** `internal/ketech/data/`, `internal/*/testdata/parity-*` (the
  export writes them) or `internal/feed/livepb/` (generated from the feed's
  protobuf schema, which isn't in this repository).
- **`queue.Queue` isn't safe for concurrent use.** `stream.Stream.run` is its
  only caller, on one goroutine. `start` callbacks must not call back into the
  queue: the stream collects started playbacks and handles them after the call
  returns.
- **A stream's queue hears the whole station.** An announcement for platforms
  outside the stream takes no lane but can still cut short or supersede one of
  the stream's. Each zone of the stream is one lane, and the mixer sums them.
- **Every stream URL carries `crs`, segment URLs included.** Production runs two
  replicas, a stream lives in one replica's memory, and the proxy routes stream
  requests by that parameter (`deploy/Caddyfile`). A new stream endpoint that
  lacks it would be sent to either replica.
- **The endless response is MP3 on purpose.** AAC silence is about 500 bytes a
  second, and Chrome never starts playing a response that slow. Don't switch it
  to AAC to save an encoder.
- **Logging** is `internal/logging`, a small logrus wrapper whose error methods
  take the error first. **Config** is Viper with validator tags. `config.toml` is gitignored.
- **`rail-announcements/` is a submodule** of the website, and only its
  `audio/` directory is read. Don't edit the website through it: work in the
  website's own checkout. To pick up new recordings, move the submodule to the
  website commit that has them.

## Commands

```sh
git submodule update --init         # the website, for its audio (about 1.7 GB)
go build ./... && go vet ./...
go test ./...                        # ffmpeg and the audio directory are optional: tests skip without them
go test ./internal/api -run Hears -v # end to end: fake feed to decoded HLS audio
go run ./cmd/rail-announcements-backend -config config.toml
```
