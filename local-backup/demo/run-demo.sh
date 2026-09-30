#!/usr/bin/env bash
# End-to-end demonstration for the local incremental backup service.
#
# Scenes:
#   1. baseline snapshot (incl. empty file, permissions, symlinks)
#   2. restore to a NEW directory; verify digest, length, mode, symlink
#   3. tiny edit -> show content-defined chunk reuse
#   4. non-empty target -> restore refuses to overwrite
#   5. fault: missing blob -> snapshot incomplete, diag names the exact chunk
#   6. fault: crash before commit -> "interrupted" row, diag from a restart
#
# Run:  ./demo/run-demo.sh            (builds to ./bin)
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/local-backup-demo.XXXXXX)"
BIN="$ROOT/bin/local-backup"
BASE="http://127.0.0.1:8765"
JGET="$ROOT/demo/jget"
chmod +x "$JGET" 2>/dev/null || true

export PATH="$HOME/sdk/go/bin:$PATH"

h1() { printf '\n\033[1;36m==== %s ====\033[0m\n' "$*"; }
h2() { printf '\n\033[1;33m-- %s --\033[0m\n' "$*"; }
say() { printf '  %s\n' "$*"; }
die() { printf '\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

api() { # api METHOD PATH [JSON_BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -X "$method" "$BASE$path" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS -X "$method" "$BASE$path"
  fi
}

start_server() { # start_server REPO_DIR [FAULT]
  local repo="$1" fault="${2:-}"
  mkdir -p "$repo"
  FAULT_INJECT="$fault" nohup "$BIN" -repo "$repo" -addr 127.0.0.1:8765 \
      >"$repo/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 50); do
    curl -sf "$BASE/v1/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$SERVER_PID" 2>/dev/null || { cat "$repo/server.log"; die "server exited early"; }
    sleep 0.1
  done
  cat "$repo/server.log"; die "server did not come up"
}
stop_server() { kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; }

trap 'stop_server 2>/dev/null; rm -rf "$WORK"' EXIT

h1 "Build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/local-backup) || die "build failed"
say "binary: $BIN"
say "workdir: $WORK"

# ---------------------------------------------------------------- dataset
DATA="$WORK/data"
mkdir -p "$DATA/projects/alpha" "$DATA/projects/beta"

# Deterministic 16 MiB payload (well above the 8 MiB chunker max, multi-chunk).
python3 - "$DATA/projects/alpha/payload.bin" <<'PYEOF'
import sys, random
random.seed(20260929)
with open(sys.argv[1], "wb") as f:
    f.write(random.randbytes(16 * 1024 * 1024))
PYEOF
printf 'release notes v1\n' > "$DATA/projects/alpha/notes.txt"
: > "$DATA/projects/alpha/EMPTY.log"                 # empty file
printf 'beta seed\n' > "$DATA/projects/beta/b.conf"
chmod 750 "$DATA/projects/beta"                      # unusual directory mode
chmod 600 "$DATA/projects/beta/b.conf"               # restrictive file mode
ln -s ../alpha/notes.txt "$DATA/projects/beta/notes-link"
ln -s /etc/hostname "$DATA/projects/escape-link"     # link pointing OUTSIDE root
DATA_SHA=$(sha256sum "$DATA/projects/alpha/payload.bin" | cut -d' ' -f1)
say "prepared dataset: 16MiB payload, empty file, mode 750 dir, symlinks (one escapes root)"

# ------------------------------------------------------------- scene 1
h1 "Scene 1  baseline backup"
REPO="$WORK/repo"
start_server "$REPO"
RESP=$(api POST /v1/backups "{\"path\":\"$DATA\"}")
echo "$RESP" | python3 -m json.tool
SNAP1=$(echo "$RESP" | bash "$JGET" snapshot_id)
[[ $(echo "$RESP" | bash "$JGET" complete) == "True" ]] || die "baseline should be complete"
say "snapshot #$SNAP1 complete"

# ------------------------------------------------------------- scene 2
h1 "Scene 2  restore into a NEW directory and verify"
RESTORED="$WORK/restored-1"
RESP=$(api POST /v1/restore "{\"snapshot_id\":$SNAP1,\"target\":\"$RESTORED\"}")
echo "$RESP" | python3 -m json.tool
[[ $(echo "$RESP" | bash "$JGET" verified) == "True" ]] || die "restore verification failed"

h2 "content / length checks"
RSHA=$(sha256sum "$RESTORED/projects/alpha/payload.bin" | cut -d' ' -f1)
[[ "$RSHA" == "$DATA_SHA" ]] && say "payload sha256 matches: ${DATA_SHA:0:16}…" || die "digest mismatch"
ORIG_LEN=$(stat -c %s "$DATA/projects/alpha/payload.bin")
REST_LEN=$(stat -c %s "$RESTORED/projects/alpha/payload.bin")
[[ "$ORIG_LEN" == "$REST_LEN" ]] && say "payload length matches: $REST_LEN bytes" || die "length mismatch"
[[ ! -s "$RESTORED/projects/alpha/EMPTY.log" && -f "$RESTORED/projects/alpha/EMPTY.log" ]] \
  && say "empty file restored as a 0-byte regular file" || die "empty file wrong"
diff -r --no-dereference "$DATA" "$RESTORED" >/dev/null \
  && say "diff -r --no-dereference: tree identical" || die "trees differ"

h2 "metadata checks"
BETA_MODE=$(stat -c %a "$RESTORED/projects/beta")
[[ "$BETA_MODE" == "750" ]] && say "directory mode 750 preserved" || die "dir mode lost: $BETA_MODE"
CONF_MODE=$(stat -c %a "$RESTORED/projects/beta/b.conf")
[[ "$CONF_MODE" == "600" ]] && say "file mode 600 preserved" || die "file mode lost: $CONF_MODE"
LINK_TGT=$(readlink "$RESTORED/projects/beta/notes-link")
[[ "$LINK_TGT" == "../alpha/notes.txt" ]] && say "symlink itself preserved (target: $LINK_TGT)" || die "symlink lost"
ESCAPE_TGT=$(readlink "$RESTORED/projects/escape-link")
[[ "$ESCAPE_TGT" == "/etc/hostname" ]] && say "out-of-root link restored as a link, never followed" || die "escape link wrong: [$ESCAPE_TGT]"

# ------------------------------------------------------------- scene 3
h1 "Scene 3  small change reuses old chunks"
# Overwrite 4 KiB at offset 8 MiB: earlier content-defined boundaries stay put.
python3 - "$DATA/projects/alpha/payload.bin" <<'PYEOF'
import sys
with open(sys.argv[1], "r+b") as f:
    f.seek(8 * 1024 * 1024)
    f.write(b"Z" * 4096)
PYEOF
RESP=$(api POST /v1/backups "{\"path\":\"$DATA\"}")
echo "$RESP" | python3 -m json.tool
SNAP2=$(echo "$RESP" | bash "$JGET" snapshot_id)
NEW=$(echo "$RESP" | bash "$JGET" new_chunks)
REUSED=$(echo "$RESP" | bash "$JGET" chunks_reused)
say "after a 4KiB edit in 16MiB: new_chunks=$NEW  chunks_reused=$REUSED"
[[ "$REUSED" -ge 3 ]] || die "expected most chunks to be reused"
say "=> unchanged content maps to the exact same content-addressed blobs"

# Restore the NEW snapshot; its digest must reflect the edit.
RESTORED2="$WORK/restored-2"
RESP=$(api POST /v1/restore "{\"snapshot_id\":$SNAP2,\"target\":\"$RESTORED2\"}")
[[ $(echo "$RESP" | bash "$JGET" verified) == "True" ]] || { echo "$RESP"; die "snapshot #2 restore failed"; }
say "snapshot #2 restored and verified independently"

# ------------------------------------------------------------- scene 4
h1 "Scene 4  restore never overwrites an existing location"
RESP=$(api POST /v1/restore "{\"snapshot_id\":$SNAP1,\"target\":\"$RESTORED\"}")
echo "$RESP" | python3 -m json.tool
echo "$RESP" | grep -q "not empty" && say "rejected: target already contains data" || die "overwrite guard failed"

# ------------------------------------------------------------- scene 5
h1 "Scene 5  missing blob: incomplete snapshot, exact chunk diagnosed"
stop_server
say "restarting server with FAULT_INJECT=skip-chunk=2 (blob for the 2nd new chunk is lost)"
start_server "$REPO" "skip-chunk=2"
# Fresh tree so every chunk counts as new.
DATA2="$WORK/data2"
mkdir -p "$DATA2"
python3 - "$DATA2/log.bin" <<'PYEOF'
import sys, random
random.seed(7)
with open(sys.argv[1], "wb") as f:
    f.write(random.randbytes(16 * 1024 * 1024))
PYEOF
RESP=$(api POST /v1/backups "{\"path\":\"$DATA2\"}")
echo "$RESP" | python3 -m json.tool
[[ $(echo "$RESP" | bash "$JGET" complete) == "False" ]] || die "faulty snapshot must be incomplete"
SNAP_BAD=$(echo "$RESP" | bash "$JGET" snapshot_id)
say "backup API succeeds but reports complete=false — no silent success"

h2 "maintainer runs diagnosis on the failed snapshot"
DIAG=$(api GET "/v1/snapshots/$SNAP_BAD/diag")
echo "$DIAG" | python3 -m json.tool
echo "$DIAG" | grep -q "missing_chunks" || die "diag should report missing chunks"
MISSING_ID=$(echo "$DIAG" | bash "$JGET" missing_chunks | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["chunk_id"])')
AFFECTED=$(echo "$DIAG" | bash "$JGET" missing_chunks | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["entries"][0])')
say "specific missing blob:  $MISSING_ID"
say "needed by entry:       $AFFECTED"
[[ "$AFFECTED" == "log.bin" ]] || die "diag must name the affected file"

# Physical confirmation: the file really is absent from the blob store.
if [[ ! -e "$REPO/blobs/${MISSING_ID:0:2}/$MISSING_ID" ]]; then
  say "confirmed on disk: blobs/${MISSING_ID:0:2}/$MISSING_ID does not exist"
fi

h2 "restore is refused preflight, nothing written"
RESP=$(api POST /v1/restore "{\"snapshot_id\":$SNAP_BAD,\"target\":\"$WORK/should-not-exist\"}")
echo "$RESP" | python3 -m json.tool
[[ -e "$WORK/should-not-exist" ]] && die "refused restore created a target" || say "no partial restore directory created"

# ------------------------------------------------------------- scene 6
h1 "Scene 6  crash before commit: interrupted row survives restart"
stop_server
DATA3="$WORK/data3"
mkdir -p "$DATA3"
python3 - "$DATA3/core.bin" <<'PYEOF'
import sys, random
random.seed(11)
with open(sys.argv[1], "wb") as f:
    f.write(random.randbytes(8 * 1024 * 1024))
PYEOF
say "starting one-shot server with FAULT_INJECT=crash-commit"
FAULT_INJECT=crash-commit "$BIN" -repo "$REPO" -addr 127.0.0.1:8765 >"$REPO/crash.log" 2>&1 &
CRASH_PID=$!
for _ in $(seq 1 50); do
  curl -sf "$BASE/v1/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
CRASH_RESP=$(curl -sS -X POST "$BASE/v1/backups" -H 'Content-Type: application/json' -d "{\"path\":\"$DATA3\"}")
wait "$CRASH_PID"; RC=$?
say "process exited with code $RC (99 = injected crash before finalization)"
say "backup response (connection cut): ${CRASH_RESP:-<empty>}"

h2 "maintainer restarts WITHOUT the fault and lists snapshots"
start_server "$REPO"
api GET /v1/snapshots | python3 -m json.tool | grep -E '"id"|"state"|"reason"'
LATEST=$(api GET /v1/snapshots | bash "$JGET" snapshots | python3 -c 'import json,sys; print(json.load(sys.stdin)[-1]["id"])')
DIAG=$(api GET "/v1/snapshots/$LATEST/diag")
echo "$DIAG" | python3 -m json.tool
VERIFY=$(echo "$DIAG" | bash "$JGET" verification)
[[ "$VERIFY" == "not_final" ]] || die "crashed snapshot must show not_final"
MCOUNT=$(echo "$DIAG" | bash "$JGET" missing_chunks | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
say "snapshot #$LATEST is interrupted; chunks already flushed: missing=$MCOUNT"
say "=> the failure is a concrete, queryable snapshot row — not an empty upload queue"

h1 "All demo scenes completed successfully"
say "repo left at: $REPO (workdir removed on exit)"
