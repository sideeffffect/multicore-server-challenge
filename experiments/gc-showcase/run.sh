#!/usr/bin/env bash
# One GC showcase run: slow users holding many keep-alive connections, scored on the tail at a fixed, moderate load.
#
#   GC="-XX:+UseZGC" HEAP=45 bash run.sh <label> [users] [think-scale] [ramp] [measure]
#
# Box-like local setup (see FINDINGS.md of the submission): Postgres 18 with the box's pgtune settings and the full
# seed in the Docker container mc-pg-box on port 5434, app and Postgres on CPUs 0-3,8-11 (4 hyper-threaded cores),
# load generators on CPUs 4-7,12-15. One client IP pair has only ~28,000 source ports, so the users are split over
# four load generators, each aimed at its own 127.0.0.x address.
set -uo pipefail
LABEL="$1"; USERS="${2:-100000}"; THINK="${3:-5}"; RAMP="${4:-180s}"; MEASURE="${5:-600s}"
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
SUB="$ROOT/submissions/scala-http4s-sideeffffect"
[ -s "$SUB/target/classpath.txt" ] || { echo "no build: run $SUB/build.sh first"; exit 1; }
OUT="${OUT:-/tmp/gc-showcase}"; mkdir -p "$OUT"
export PGHOST=127.0.0.1 PGPORT=5434 PGUSER=postgres PGPASSWORD=postgres
DB="gcs_$$"
psql -X -q -d postgres -c "CREATE DATABASE $DB TEMPLATE conduit_seed STRATEGY FILE_COPY"
docker exec mc-pg-box psql -U postgres -d "$DB" -q -c "VACUUM ANALYZE" >/dev/null
export DATABASE_URL="postgres://conduit:conduit@127.0.0.1:5434/$DB" JWT_SECRET=multicore-server-challenge HOST=0.0.0.0 PORT=3100
ulimit -n 1000000 2>/dev/null || ulimit -n 200000
GCLOG="$OUT/$LABEL.gc.log"; rm -f "$GCLOG"
taskset -c 0-3,8-11 systemd-run --user --scope -q -p MemoryMax=16G -p MemorySwapMax=0 \
  /tmp/jdk-27/bin/java ${GC:--XX:+UseParallelGC} -XX:MaxRAMPercentage="${HEAP:-45}" -XX:InitialRAMPercentage="${HEAP:-45}" \
  -XX:+AlwaysPreTouch -XX:-UsePerfData -Dcats.effect.tracing.mode=none --sun-misc-unsafe-memory-access=allow \
  -Xlog:gc:file="$GCLOG" -cp "$(cat "$SUB/target/classpath.txt")" conduit.Main > "$OUT/$LABEL.app.log" 2>&1 &
PID=$!
for _ in $(seq 90); do curl -sf -o /dev/null http://127.0.0.1:3100/api/tags && break; sleep 1; done
curl -sf -o /dev/null http://127.0.0.1:3100/api/tags || { echo "server didn't start:"; tail -5 "$OUT/$LABEL.app.log"; exit 1; }

PER=$((USERS / 4)); START=$(( $(date +%s%3N) + 5000 ))
for i in 0 1 2 3; do
  taskset -c 4-7,12-15 systemd-run --user --scope -q -p MemoryMax=8G -p MemorySwapMax=0 \
    /tmp/benchmc/loadgen run -target "http://127.0.0.$((i + 1)):3100" -users "$PER" -box-index "$i" \
    -id-offset $((i * PER)) -start-at "$START" -ramp "$RAMP" -measure "$MEASURE" -tail 10s \
    -secret "$JWT_SECRET" -run-id "gcs$$" -articles 500000 -think-scale "$THINK" -q -out "$OUT/$LABEL.box$i.json" \
    > /dev/null 2>&1 &
done

# CPU and memory over the measured window only
sleep $(( 5 + ${RAMP%s} + 10 ))
PGCG=/sys/fs/cgroup/system.slice/docker-$(docker inspect -f '{{.Id}}' mc-pg-box).scope/cpu.stat
a0=$(awk '{print $14+$15}' /proc/$PID/stat); p0=$(awk '/usage_usec/{print $2}' "$PGCG"); s0=$(date +%s.%N)
sleep $(( ${MEASURE%s} - 20 ))
a1=$(awk '{print $14+$15}' /proc/$PID/stat); p1=$(awk '/usage_usec/{print $2}' "$PGCG"); s1=$(date +%s.%N)
CONNS=$(ls /proc/$PID/fd | wc -l); RSS=$(( $(ps -o rss= -p $PID) / 1024 ))
wait $(jobs -p | grep -v "^$PID$") 2>/dev/null

/tmp/benchmc/loadgen merge -o "$OUT/$LABEL.merged.json" "$OUT/$LABEL".box?.json > "$OUT/$LABEL.merge.txt" 2>&1
kill $PID; wait $PID 2>/dev/null
psql -X -q -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)"

python3 - "$OUT/$LABEL.merged.json" "$a0" "$a1" "$p0" "$p1" "$s0" "$s1" "$CONNS" "$RSS" "$LABEL" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1])); o = d['overall']
a0, a1, p0, p1, s0, s1 = map(float, sys.argv[2:8]); conns, rss, label = sys.argv[8], sys.argv[9], sys.argv[10]
dt = s1 - s0
print(f"{label}: req/s={o['rps']:.0f} p95={o['p95_ms']:.1f} p99={o['p99_ms']:.1f} err={o['error_rate']*100:.2f}% "
      f"app={(a1-a0)/100/dt:.2f} cores pg={(p1-p0)/1e6/dt:.2f} cores open-fds={conns} rss={rss}MB "
      f"lg-limited={d['verdict'].get('lg_limited')}")
EOF
/tmp/benchmc/tailq "$OUT/$LABEL.merged.json"
python3 - "$GCLOG" <<'EOF'
import re, sys
pauses = []; full = 0
for line in open(sys.argv[1]):
    m = re.search(r'Pause.*?([\d.,]+)ms$', line.strip())
    if m:
        pauses.append(float(m.group(1).replace(',', '.')))
        full += 'Full' in line
pauses.sort()
if pauses:
    print(f"GC pauses: {len(pauses)} (full: {full}), total {sum(pauses)/1000:.2f} s, max {pauses[-1]:.1f} ms, "
          f"p99 {pauses[int(len(pauses)*0.99)-1]:.1f} ms")
else:
    print("GC pauses: none logged")
EOF
