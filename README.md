# rail-announcements-backend

A Go service that speaks station announcements. It listens to
the announcement stream of Darwin Browser (darwinbrowser.com), builds each
announcement from the recordings that
[railannouncements.co.uk](https://github.com/davwheat/rail-announcements) uses, and serves the result as
an HTTP Live Stream (HLS). It also renders a single announcement from a posted
website state, which is how every announcement system on the website moves to
this service over time.

The announcement logic is a port of the website's TypeScript. The website stays
the reference, and the tests here fail on any clip that differs from it. See
[Keep the port in step with the website](#keep-the-port-in-step-with-the-website).

For the design and the migration plan, see
[docs/architecture.md](docs/architecture.md).

## Run the service

You need Go 1.26 and `ffmpeg` on your `PATH`.

1.  Fetch the website, which holds the audio. It's a submodule at
    `rail-announcements`, and the service reads `rail-announcements/audio`:

    ```sh
    git submodule update --init
    ```

    The website's repository is about 1.7 GB. If you already have a clone, add
    `--reference PATH_TO_CLONE` to borrow its objects instead of downloading
    them. Replace `PATH_TO_CLONE` with the path of your clone.

1.  Optional: copy `config.example.toml` to `config.toml` and edit it. Every
    key has a default.

1.  Start the service:

    ```sh
    go run ./cmd/rail-announcements-backend
    ```

The feed must speak protocol version 2 of the station streams. The service
refuses any other version and retries.

## Live streams

```
GET /v1/streams/live.m3u8?crs=KGX&zone=1:phil,2:celia&zone=3:phil
GET /v1/streams/live.mp3?crs=KGX&zone=1:phil,2:celia&zone=3:phil
```

A stream is one listener's mix of a station. It's made of *announcement zones*:
the platforms that share a loudspeaker and so take turns. Zones speak at the
same time, and the stream is their sum. Listeners who ask for the same zones
with the same choices share one stream. A stream starts on its first request
and stops 45 seconds after its last one.

Both URLs take the same parameters and carry the same audio:

- `live.m3u8` is an HLS playlist of one-second AAC segments. Safari plays it
  natively, and so does any HLS player.
- `live.mp3` is one endless response of constant bit rate MP3, the way internet
  radio is served. Every browser plays it from a plain `<audio>` element with
  no script, so it keeps playing in a background tab. It opens with the last
  three seconds of audio so that a player starts at once. It's MP3 and not AAC
  because AAC spends about 500 bytes a second on silence, and a browser that is
  waiting for enough bytes to recognize the format would take minutes to start.

A listener hears an announcement about two seconds after the service starts it,
on either URL. The first listener of an MP3 stream that isn't running yet is
about four seconds behind until they reconnect.

| Parameter | Value | Default |
|---|---|---|
| `crs` | The station's CRS code. Required. | |
| `zone` | One zone: its platforms separated by commas, each as `NUMBER` or `NUMBER:VOICE`. Repeat it for each zone. Without it, the whole station is one zone. | Every platform |
| `platform` | Shorthand for a stream of one zone: a platform of that zone. Repeat it, or separate platforms with commas. It can't be combined with `zone`. | |
| `voice` | The voice for platforms that name none: `phil` or `celia`, or the website's system ID. | `phil` |
| `type` | The announcements to make: `next`, `approaching`, `standing`, `disrupted`, `passing`, `platform_alteration`. | All of them |
| `chime` | `three`, `four` or `none`. | The voice's own |
| `vias` | Whether to announce via points. | `true` |
| `legacy_tocs` | Whether to use the operators' former names. | `false` |
| `short_platforms_after_split` | Whether to announce short platforms for each portion of a dividing train. | `false` |
| `fast_train_approaching` | Whether a fast train warning ends with "fast train approaching". | `false` |
| `fanfare` | Whether a fast train warning starts with the Daktronics fanfare. | `false` |
| `missing_audio` | What to do when a recording is missing: `skip-service`, `play-silence`, `repeat-last-station` or `repeat-last`. | `skip-service` |

## Render one announcement

```
POST /v1/announcements
Content-Type: application/json

{"system": "AMEY_PHIL_V1", "announcement": "nextTrain", "state": { ... }}
```

`state` is the option state of the website tab named by `announcement`,
unchanged. The response is the MP3 (`audio/mpeg`), or a JSON error:

```json
{"error": {"code": "missing_audio", "message": "audio file not found: station/ketech/phil/station/e/ZZZ.mp3"}}
```

| Status | Code | Meaning |
|---|---|---|
| 400 | `bad_request` | The body isn't the JSON object shown. |
| 404 | `unknown_system`, `unknown_announcement` | `GET /v1/systems` lists what exists. |
| 422 | `invalid_state` | The state can't be read, or describes an announcement that can't be made. |
| 422 | `missing_audio` | A recording doesn't exist, and the state's `missingAudioMode` doesn't allow for that. |
| 422 | `empty_announcement` | The state describes nothing to say. |
| 500 | `render_failed` | The audio couldn't be produced. |

`GET /healthz` reports the number of running streams.

## Test

```sh
go test ./...
```

Tests that need `ffmpeg` or the audio directory skip themselves when either is
missing. `internal/api` holds the end-to-end test: a fake feed sends a
protobuf announcement, and the test decodes the HLS segments and checks that
they carry speech.

## Keep the port in step with the website

`internal/ketech` and `internal/queue` are ports of the website's AmeyPhil and
AmeyCelia systems, its live announcement logic (`src/live/playAnnouncement.ts`)
and its playback queue (`src/live/playbackQueue.ts`). The website generates what
the ports are tested against:

```sh
npm run export:backend -- PATH_TO_THIS_REPOSITORY
```

Run it in a checkout of the website that has the change, and replace
`PATH_TO_THIS_REPOSITORY` with the path of this repository. Don't run it in the
`rail-announcements` submodule here, which is pinned to a commit and is only
read for its audio.

That command writes the following files:

- `internal/ketech/data/*.json`: the voices' tables, such as operators,
  platforms, delay reasons and short platforms. The service embeds them.
- `internal/ketech/testdata/parity-*.json.gz`: the clips the website plays for
  about 2,700 live announcements and 800 tab states, and its answers for
  operator names and short platforms. The live announcements are built from
  `movements.json.gz`, which holds real movements captured from production.
- `internal/queue/testdata/parity-queue.json`: scripts run against the
  website's queue, with everything the queue did at each step.

To change how an announcement is worded, change the website, run the export,
and then port the change until `go test ./...` passes.

## The protobuf code

`internal/feed/livepb` is generated from the feed's protobuf schema, which isn't
part of this repository. The generated code is committed, so you don't need the
schema to build or test. `buf.gen.yaml` holds the generator settings for
whoever has the schema.
