# Production deployment

Two replicas of the service behind a proxy, run by rootless Docker. One replica
is always serving, including during a rollout.

```
host TLS proxy ──▶ 127.0.0.1:11000  proxy (Caddy)
                                     ├─▶ backend-1   (also 127.0.0.1:11001)
                                     └─▶ backend-2   (also 127.0.0.1:11002)
```

## How requests are routed

A stream is held in one replica's memory: its queue, its encoders and its
segments. A listener's playlist, segments and endless MP3 response must
therefore all reach the same replica.

Every stream URL carries the station as `crs`, including each segment URL in a
playlist, and the proxy hashes stream requests on it. Hashing on the station,
and not on the listener, also puts all of a station's listeners on one replica.
That replica then holds one feed connection for the station and one encoder for
each distinct stream, however many people listen.

`POST /v1/announcements` needs nothing from an earlier request, so it goes to
the replicas in turn.

The proxy checks each replica's `/healthz` every two seconds and hashes over
the replicas that are up. When a replica goes down, its stations move to the
other replica, and they move back when it returns. A listener's player sees
each move as a stream that ended, and reconnects: the website's player does so
after three seconds. A request that arrives while a replica is being replaced
is retried on the other one for up to five seconds.

## Set up the host

Do this once, as the user that runs the service.

1.  Install rootless Docker for that user. See
    [Rootless mode](https://docs.docker.com/engine/security/rootless/) in the
    Docker documentation.

1.  Keep the user's Docker daemon running when nobody is logged in, and start
    it at boot. Without this, the daemon and every container stop when the
    user's last session ends:

    ```sh
    sudo loginctl enable-linger "$USER"
    systemctl --user enable --now docker
    ```

1.  Point the Docker CLI at the rootless daemon, in the user's shell profile:

    ```sh
    export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/docker.sock
    ```

1.  Clone the repository with its submodule, which holds the audio:

    ```sh
    git clone --recurse-submodules REPOSITORY_URL
    ```

    Replace `REPOSITORY_URL` with this repository's URL. The submodule is about
    1.7 GB.

1.  Put a TLS proxy in front of `127.0.0.1:11000`. Rootless Docker can't publish
    a port below 1024, so the stack doesn't terminate TLS itself. The proxy in
    front must not buffer responses, because `live.mp3` is one response that
    never ends. For nginx:

    ```nginx
    location / {
        proxy_pass http://127.0.0.1:11000;
        proxy_http_version 1.1;
        proxy_buffering off;
    }
    ```

    The service also sends `X-Accel-Buffering: no` on that response, which
    nginx honors without the `proxy_buffering` line.

    Leave `proxy_read_timeout` at its default of 60 seconds. It isn't a limit
    on how long a response lasts: it's the longest wait between two reads from
    the service, and the MP3 response sends a frame about every 26
    milliseconds, silence included. It only fires when a replica hangs without
    closing the connection, and then a short timeout is what gets the listener
    reconnected. A proxy or CDN that does cap a response's total duration
    would cut listeners off at that limit, and their players would reconnect.

### What rootless mode changes

- **File ownership.** The containers run as user ID 10001, which rootless
  Docker maps to one of the host user's subordinate IDs, not to the host user.
  The audio is mounted read-only, so it only has to be readable by everyone,
  which a Git checkout is.
- **Nothing is written.** Each container's file system is read-only. The
  replicas get a `/tmp` in memory, where ffmpeg leaves a rendered MP3 for as
  long as it takes to read it back.
- **No resource limits.** The stack sets none, because rootless Docker only
  enforces them when cgroup v2 delegation is set up for the user. To cap
  memory, set that up first, and then add limits to the `x-replica` block.
  `AUDIO_CACHE_MB` and `MAX_STREAMS` bound what a replica uses in the meantime.

## Deploy and update

```sh
./deploy/deploy.sh
```

The script pulls `main` and the submodule, builds the image, and then replaces
`backend-1` and `backend-2` in turn. It waits for each to report healthy, and
then ten more seconds for the proxy to send its stations back, before it
touches the next one. The same command performs the first deploy.

- If a replica doesn't become healthy in 90 seconds, the script prints its logs
  and stops. The other replica is still serving the previous version.
- If either replica is unhealthy before the rollout starts, the script refuses
  to start, because replacing the healthy one would leave nothing serving.
- To deploy what is checked out without pulling, pass `--no-pull`.

During a rollout, a listener can hear up to two gaps of a few seconds: one when
their station's replica is replaced, and one when the station moves back. An
announcement that is being spoken at that moment is cut off, and isn't repeated.

Changing `Caddyfile` recreates the proxy at the end of the rollout, which drops
every connection for about a second.

## Settings

Set these in the environment, or in a `.env` file in this directory.

| Variable | Meaning | Default |
|---|---|---|
| `LISTEN` | The host address and port that the proxy is published on. Send listeners here. | `127.0.0.1:11000` |
| `LISTEN_BACKEND_1`, `LISTEN_BACKEND_2` | Where each replica is published by itself, for looking at one replica. | `127.0.0.1:11001`, `127.0.0.1:11002` |
| `DARWIN_BROWSER_URL` | The feed. | `https://darwinbrowser.com` |
| `ALLOWED_ORIGINS` | The origins that may call `POST /v1/announcements`, separated by commas. Playback needs no permission. | `https://railannouncements.co.uk` |
| `AUDIO_CACHE_MB` | Decoded clips held in memory, for each replica. | `256` |
| `MAX_STREAMS` | Streams that one replica runs at once. | `200` |

## Look at it

```sh
docker compose -f deploy/docker-compose.yml ps
docker compose -f deploy/docker-compose.yml logs -f backend-1 backend-2
curl -s http://127.0.0.1:11001/healthz   # backend-1
curl -s http://127.0.0.1:11002/healthz   # backend-2
```

`/healthz` on port 11000 goes through the proxy and answers from either
replica, so ask a replica's own port for its `streams` count. Don't point
listeners at those ports: only the proxy keeps a stream's requests on the
replica that holds it.
