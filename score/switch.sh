#!/usr/bin/env bash
# Puts the benchmark box into a clean, identical state for ONE submission. Run as root on the box
# (installed at /opt/conduit-tools/switch.sh by score/install-tools.sh; `loadgen search -lang <name>` calls it).
#
#   switch.sh rust-hyper-cknutson12                  # conformance on the full seed, then a fresh DB and start
#   switch.sh rust-hyper-cknutson12 --no-conformance
#   CONDUIT_TRACK=tail switch.sh rust-hyper-cknutson12   # tail-latency track (README.md); start.sh sees $CONDUIT_TRACK
#
# The submission must already be in /opt/challenge/subs/<name>, installed and built (score/prepare.sh).
# Steps: stop challenge.service -> Front from the README (nginx: Nginx on :80 -> app on 127.0.0.1:3000;
# direct: Nginx stopped, app on 0.0.0.0:80) -> /opt/challenge/current -> /etc/challenge/env -> fresh `conduit`
# DB from `conduit_seed` -> [conformance, stop, fresh DB again] -> restart Postgres -> drop the page cache
# -> start -> wait for GET /api/tags (must answer within 60 s, rule 11) -> print env, idle memory.
set -euo pipefail
NAME="${1:?usage: switch.sh <submission folder name> [--no-conformance]}"
CONFORMANCE=1; [ "${2:-}" = --no-conformance ] && CONFORMANCE=0
SUB="/opt/challenge/subs/$NAME"
UNIT=challenge.service
CONF_BIN=/opt/conduit-tools/conformance
JWT_SECRET=multicore-server-challenge
TRACK="${CONDUIT_TRACK:-standard}"
case "$TRACK" in standard|tail) ;; *) echo "unknown CONDUIT_TRACK=$TRACK (standard or tail)"; exit 1 ;; esac
say() { printf '[switch %s %s] %s\n' "$NAME" "$(date +%T)" "$*"; }
[ -f "$SUB/start.sh" ] || { echo "no $SUB/start.sh (score/prepare.sh $NAME first)"; exit 1; }
[ -f "$SUB/.built" ] || { echo "$SUB not built (score/prepare.sh $NAME first)"; exit 1; }
PSQL="runuser -u postgres -- psql -qX -v ON_ERROR_STOP=1"

FRONT="$(grep -ioE '^[*_ ]*front[*_ ]*:[*_ ]*(nginx|direct)' "$SUB/README.md" | head -1 | grep -ioE '(nginx|direct)$' | tr 'A-Z' 'a-z')"
case "$FRONT" in
  nginx) HOST=127.0.0.1; PORT=3000 ;;
  direct) HOST=0.0.0.0; PORT=80 ;;
  *) echo "README.md has no 'Front: nginx|direct' line"; exit 1 ;;
esac

fresh_db() {
  # FILE_COPY: a plain file copy of the template (WAL_LOG would push the whole DB through shared_buffers + WAL)
  $PSQL -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('conduit', 'conduit_seed') AND pid <> pg_backend_pid()" >/dev/null
  $PSQL -c "DROP DATABASE IF EXISTS conduit WITH (FORCE)" \
        -c "CREATE DATABASE conduit TEMPLATE conduit_seed STRATEGY FILE_COPY" >/dev/null
}
wait_ready() {
  local t0=$SECONDS
  while [ $((SECONDS - t0)) -lt 60 ]; do
    curl -sf -m 2 -o /dev/null http://127.0.0.1/api/tags && return 0
    systemctl is-active -q "$UNIT" || break
    sleep 0.5
  done
  say "NOT READY after $((SECONDS - t0)) s (unit $(systemctl is-active "$UNIT"))"; journalctl -u "$UNIT" -n 40 --no-pager; exit 1
}
stop_app() {
  systemctl stop "$UNIT" 2>/dev/null || true
  systemctl reset-failed "$UNIT" 2>/dev/null || true
  for _ in $(seq 1 40); do ss -ltnH '( sport = :3000 )' | grep -q . || return 0; sleep 0.5; done
  say "something still listens on :3000"; ss -ltnp '( sport = :3000 )'; exit 1
}

say "host: $(hostname), $(nproc) vCPU, $(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo) MiB, kernel $(uname -r); Front: $FRONT"
say "stopping $UNIT"; stop_app
systemctl start postgresql@18-main
if [ "$FRONT" = nginx ]; then
  systemctl start nginx
else
  systemctl stop nginx
  for _ in $(seq 1 20); do ss -ltnH '( sport = :80 )' | grep -q . || break; sleep 0.5; done
  if ss -ltnH '( sport = :80 )' | grep -q .; then say "port 80 still taken"; ss -ltnp '( sport = :80 )'; exit 1; fi
fi

ln -sfn "$SUB" /opt/challenge/current
umask 027
{
  echo "DATABASE_URL=postgres://conduit:conduit@127.0.0.1:5432/conduit"
  echo "JWT_SECRET=$JWT_SECRET"
  echo "HOST=$HOST"
  echo "PORT=$PORT"
  echo "CONDUIT_TRACK=$TRACK"
} > /etc/challenge/env.new
chgrp challenge /etc/challenge/env.new && mv /etc/challenge/env.new /etc/challenge/env
umask 022

say "fresh database conduit from conduit_seed"; t=$(date +%s); fresh_db; say "  done in $(( $(date +%s) - t )) s"

if [ "$CONFORMANCE" = 1 ]; then
  say "conformance on the full seed, through port 80"
  systemctl start "$UNIT"; wait_ready
  if ! "$CONF_BIN" -base http://127.0.0.1 -secret "$JWT_SECRET" -q > "/root/conformance-$NAME.txt" 2>&1; then
    cat "/root/conformance-$NAME.txt"; say "CONFORMANCE FAILED: not benchmarking this"; stop_app; exit 1
  fi
  tail -3 "/root/conformance-$NAME.txt"
  stop_app
  say "reset database after conformance"; fresh_db
fi

say "restart Postgres, drop page cache (every run starts equally cold)"
systemctl restart postgresql@18-main
for _ in $(seq 30); do runuser -u postgres -- pg_isready -q && break; sleep 1; done
sync; echo 3 > /proc/sys/vm/drop_caches

say "starting $UNIT"; t0=$(date +%s%N)
systemctl start "$UNIT"; wait_ready
say "ready after $(( ($(date +%s%N) - t0) / 1000000 )) ms"
sleep 3
cgdir="/sys/fs/cgroup$(systemctl show -p ControlGroup --value "$UNIT")"
pss=0
for p in $(cat "$cgdir/cgroup.procs"); do
  k=$(awk '$1=="Pss:"{print $2}' "/proc/$p/smaps_rollup" 2>/dev/null); pss=$((pss + ${k:-0}))
done
echo "---- /etc/challenge/env"; grep -v '^JWT_SECRET' /etc/challenge/env
echo "---- idle app memory: PSS $((pss / 1024)) MB, $(wc -l < "$cgdir/cgroup.procs") process(es); nginx $(systemctl is-active nginx)"
echo "---- $(systemctl status "$UNIT" --no-pager -n0 | sed -n '1p;3p' | xargs)"
say "READY"
