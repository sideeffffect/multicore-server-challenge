# bench

Go module `conduitbench`: the deterministic seed (`seed/`, `cmd/seedgen`), the API conformance suite
(`cmd/conformance`) and the load generator (`cmd/loadgen`).

```sh
cd bench
go build -o loadgen ./cmd/loadgen                          # Mac
GOOS=linux GOARCH=amd64 go build -o loadgen ./cmd/loadgen  # load boxes
go test ./cmd/loadgen                                      # user model vs WORKLOAD.md mix, against a stub server
```

## loadgen

One binary with three subcommands. Each virtual user (VU) is one goroutine with **its own keep-alive TCP
connection**, following the user model in `../WORKLOAD.md`:

- think time exp(mean 7 s, cap 70 s); session length exp(mean 15 min, cap 60 min), then a new session;
- session start: 75% `GET /api/user` with a minted token (HS256, `sub`/`iat`/`exp = +30 d`), 24.5% login,
  0.5% sign-up (`lg<run-id>b<box>n<counter>`, then browses as that user);
- page views drawn by the WORKLOAD.md weights, making the API calls listed there. Offsets are page 1 70% of the
  time, else pages 2–5. Articles come 80% from the last list seen and 20% from a deep link, Zipf(0.9) over recency
  (`id = N - rank`). Tags come 50% from the current article and 50% Zipf over all tags. Profile and follow use the
  current article's author. Favorite and follow toggle on the `favorited` / `author.following` flags;
- a page that needs a current article when there isn't one opens an article first and does that page next.
  "Edit own article" and "delete own comment" without anything of your own this session become an article view.
  Following yourself also becomes an article view.

Users arrive evenly over `-ramp`, each after its own random think, and run through ramp + measure + tail. Only
requests that **start** inside the measure window go into the per-endpoint HdrHistograms (1 µs–60 s, 3 significant
figures), status counts and error classes. Failed requests count too: timeouts at 10 s, connection errors, and
non-2xx/3xx responses (`http_<code>`). Each request's latency includes one transparent retry, the way browsers
retry when the server closed an idle keep-alive connection just as the request went out.

The HTTP client is a small HTTP/1.1 client (`client.go`). It writes the request bytes itself, parses the response
with `net/http.ReadResponse`, and borrows its buffers from pools only while a request is in flight, so an idle user
holds just a goroutine and a socket. It sends `Accept-Encoding: gzip` but decompresses only the bodies the user
reads (lists, articles, the login/sign-up token, a new comment's id). Every other body is drained unread.

### `loadgen run` (on each load box)

```sh
loadgen run -target http://10.0.0.2 -users 4000 -box-index 0 -id-offset 0 \
  -ramp 60s -measure 60s -tail 15s -start-at <unix ms> -secret "$JWT_SECRET" -run-id go-probe-10000 -out box0.json
```

| flag | meaning |
|---|---|
| `-users` | VUs on this box. VU global index `g = id-offset + i` maps to seeded user `g mod 100000 + 1` |
| `-box-index` / `-id-offset` | keep boxes' users disjoint (search sets them) |
| `-start-at` | unix ms at which the ramp starts; every box gets the same value (0 = now + 2 s) |
| `-ramp` / `-measure` / `-tail` | arrival period, measured window, and how long users keep going after the window |
| `-secret` | the target's `JWT_SECRET` (or `$CONDUIT_JWT_SECRET`) |
| `-run-id` | must differ between runs on the same database (it's part of sign-up usernames) |
| `-articles` | seeded article count for deep links (500000; **10000 for the local dev DB**) |
| `-track` | scoring track: `standard` (default) or `tail` (5× think time and session length, tail criteria; README.md, "Tail-latency track") |
| `-think-scale` | **testing only**: multiplies think times *and* session lengths (default: the track's pace, 1 or 5) |
| `-q` | don't print the summary table to stderr |

The output JSON has:
- `meta`;
- `endpoints` (per label: count, statuses, error classes, an example error, and the histogram as an HdrHistogram V2
  compressed base64 string);
- `pages` and `session_starts`, counted in the window;
- `box`: box CPU avg/max over the window from `/proc/stat` (`null` on macOS), the loadgen process's CPU, peak RSS,
  TCP connects, and `fell_behind`;
- `timeline`, one entry per second of the whole run: started/completed/errors, active VUs, p50/p95/p99 (plus that
  second's encoded histogram, so boxes merge exactly), timer wake-up lag, box CPU, process CPU, RSS and goroutines.

The box sets `fell_behind` when more than 5% of the window's seconds have a mean think-timer wake-up lag over
50 ms, when the sampler's 1 s tick runs more than 500 ms late, or when fewer than 99% of the users are active at
the window start.

Load boxes need `../box/sysctl.conf`: `ip_local_port_range 1024 65535` matters, since each user holds
one connection to the same target ip:port. They also need a high `nofile`. Go raises the soft limit to the hard
limit, and search runs `ulimit -n 1048576` first.

### `loadgen merge`

```sh
loadgen merge -o merged.json box0.json box1.json box2.json   # exit 0 pass, 3 fail
```

Merges histograms, counts and timelines exactly. It gives each endpoint and overall: count, req/s (over the
measure window), p50/p90/p95/p99/max/mean and error rate. It also gives the request mix (calls per page view,
req/s per user, write %, page shares), the verdict, and a table:

- **PASS**, on the standard track, iff overall p95 < 500 ms, p99 < 1000 ms and errors < 1%; on the tail track, iff
  p99 < 250 ms, p99.9 < 500 ms, max < 2000 ms and errors < 0.1%. The track comes from the boxes' `meta.track`; boxes
  on different tracks never pass.
- **`lg_limited`** if any box's average CPU over the window is above 75%, or any box `fell_behind`.

### `loadgen search` (the orchestrator I score with; needs root ssh to the box and the load boxes)

```sh
export CONDUIT_JWT_SECRET=...        # the target's JWT_SECRET
loadgen search -target-ssh root@10.0.0.2 -lang go -outdir results/go -dry   # print the plan
loadgen search -target-ssh root@10.0.0.2 -lang go -outdir results/go
```

Defaults: `-target-url http://<target-ssh host>`, `-boxes local=1` (e.g. `root@10.0.0.3=4,root@10.0.0.4=4,local=8`),
`-ssh-key ""` (ssh's default identity), `-start 500`, `-max 120000`, `-resolution 0.05`.
`-track tail` switches the pace, the criteria and the default `-probe-measure` / `-hold-measure` (3 min / 10 min)
and passes `-track` to every box; set `-target-url` to several comma-separated addresses so no box runs out of
source ports.

1. Preflight. ssh to the target (also reads its timezone) and to each load box. The binary copies itself to
   `-remote-bin` (`/usr/local/bin/conduit-loadgen`) wherever the sha256 differs.
2. `ssh target /opt/conduit-tools/switch.sh <lang>` runs once per search (fresh DB, caches dropped, app healthy).
   Output goes to `switch.log`, and `switch.ok` marks it done.
3. Warm-up: `-start` users, 60 s ramp + 120 s.
4. Probes: 60 s ramp + 60 s measured. Users double from `-start` until a probe fails, then bisect until
   `fail - pass <= 5% of pass`. Values are rounded to 2 significant digits. If the start already fails, the user
   count halves until a probe passes.
5. Holds: 60 s ramp + 300 s measured at the highest passing probe, stepping down 5% until a hold passes. That hold
   is the **score**.

Every phase:
- waits at least 5 s, then until target CPU < `-idle-cpu` 15% (at most `-idle-max` 90 s). The DB is not reset;
- checks target health;
- starts `/opt/conduit-tools/monitor.sh 1 <label>-<phase>` on the target (cwd `-monitor-dir`, own session);
- sets `start-at = now + 5 s` and launches `loadgen run` detached on every box (`setsid nohup`, guarded by a
  `mkdir` so a retried launch can't start it twice). It polls for the exit code and fetches the JSON with `cat`;
- stops the monitor and fetches its CSV. It looks for `*<tag>*.csv` in `-monitor-dir`,
  `/opt/conduit-tools/results` and `/root/results`, and keeps the monitor's stdout if there's no CSV;
- merges and appends a one-line summary to `<outdir>/search.log`.

Each ssh call retries once on an ssh failure (exit 255).

**LG-limited:** if a probe or hold is `lg_limited`, the search stops and reports "LG-limited at N". That is never
treated as a server failure.

Output in `<outdir>/`:
- `search.log`;
- `switch.log`;
- `<phase>/box<i>.json|.log`, `<phase>/merged.json|.txt` and `<phase>/monitor.csv`, where phase is `warmup`,
  `probe-<N>` or `hold-<N>`;
- `SUMMARY.json` and `SUMMARY.txt`: status (`ok` / `lg_limited` / `no_pass`), score users, req/s, p50/p95/p99,
  errors, the per-endpoint table, load-box CPU, every probe and hold, and the target's CPU over the hold window
  from the monitor CSV.

The CSV is matched on its `time` column. That column can be unix s/ms, RFC 3339, or `HH:MM:SS` in the target's
timezone. Every numeric column is averaged, and `cpu%`, `steal%`, `app%`, `db%` and `nginx%` are picked out.

**Resuming:** run the same command again. Phases with a `merged.json` are loaded rather than re-run. The search
replays the same decisions, and `switch.ok` skips the DB reset. Delete a phase directory to redo that phase, or
`switch.ok` to reset again.

Testing switches: `-target-ssh local` runs the target commands with local bash. A box named `bash` goes through the
remote code path on this machine (`-push-bin=false -remote-bin /path/to/loadgen`). Use `-no-switch`, `-monitor ''`
and `-think-scale` for these tests.

### Load test your own server locally

```sh
psql -d postgres -c 'CREATE DATABASE conduit_lg TEMPLATE conduit_dev'
DATABASE_URL=postgres://conduit:conduit@127.0.0.1:5432/conduit_lg JWT_SECRET=dev-secret \
  HOST=127.0.0.1 PORT=3110 bash ../submissions/<yours>/start.sh &
go run ./cmd/loadgen run -target http://127.0.0.1:3110 -users 3000 -ramp 10s -measure 30s -tail 5s \
  -secret dev-secret -articles 10000 -think-scale 0.2 -out /tmp/local.json
```

macOS has only ~16k ephemeral ports, and runs that end leave their sockets in TIME_WAIT. Keep local runs under
~15k users and wait ~30 s between big runs.
