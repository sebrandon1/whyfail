#!/usr/bin/env bash
# Regenerate testdata/<corpus> from live Prow/GCS artifacts.
#
# Only public artifacts are fetched. Bodies are stored exactly as GCS stores
# them (gzip when the object is gzip-encoded, identity otherwise) so the
# listing's `size` and the on-disk file size agree -- that is what
# `Artifacts.TotalBytes` is computed from.
#
# Usage: scripts/fetch-fixtures.sh
set -euo pipefail

BUCKET=test-platform-results-public
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/testdata"

# corpus|name|run prefix (pr-logs/.../<job>/<run-id>)
RUNS=(
  "recert-2364|ipc|pr-logs/pull/rh-ecosystem-edge_recert/2364/pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow/2120987688944275456"
  "recert-2364|serial|pr-logs/pull/rh-ecosystem-edge_recert/2364/pull-ci-rh-ecosystem-edge-recert-main-e2e-aws-ovn-single-node-recert-serial/2120987680551473152"
  "recert-2364|baremetal|pr-logs/pull/rh-ecosystem-edge_recert/2364/pull-ci-rh-ecosystem-edge-recert-main-baremetalds-sno-recert-cluster-rename/2120987667142283264"
  "cert-manager-533|e2e-operator|pr-logs/pull/openshift_cert-manager-operator/533/pull-ci-openshift-cert-manager-operator-master-e2e-operator/2121292213823475712"
)

# corpus|job, for the presubmit run index that history is built from.
INDEX_JOBS=(
  "recert-2364|pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow"
)

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

list_objects() { # <prefix> <out.json>
  python3 - "$BUCKET" "$1" "$2" <<'PY'
import json, sys, urllib.parse, urllib.request
bucket, prefix, out = sys.argv[1], sys.argv[2], sys.argv[3]
items, token = [], None
while True:
    q = {"prefix": prefix, "maxResults": "1000"}
    if token:
        q["pageToken"] = token
    url = ("https://storage.googleapis.com/storage/v1/b/%s/o?" % bucket) + urllib.parse.urlencode(q)
    with urllib.request.urlopen(url, timeout=60) as r:
        page = json.load(r)
    items += [{"name": i["name"], "size": i["size"], "updated": i.get("updated", "")}
              for i in page.get("items", [])]
    token = page.get("nextPageToken")
    if not token:
        break
json.dump({"items": items}, open(out, "w"))
PY
}

fetch_object() { # <object> <dest> [expected stored size]; returns 1 on miss
  local object="$1" dest="$2" want="${3:-}" code size attempt
  mkdir -p "$(dirname "$dest")"
  for attempt in 1 2 3 4 5 6 7 8; do
    # Ask for the stored encoding without letting curl expand it, so the saved
    # bytes match the listing's `size` exactly. GCS occasionally ignores the
    # header and transcodes, so verify and retry rather than store the wrong
    # form (which would make Artifacts.TotalBytes lie).
    code=$(curl -sS -H 'Accept-Encoding: gzip' -o "$dest" -w '%{http_code}' \
      "https://storage.googleapis.com/$BUCKET/$object")
    if [ "$code" != "200" ]; then
      rm -f "$dest"
      echo "  miss $code $object" >&2
      return 1
    fi
    [ -z "$want" ] && return 0
    size=$(wc -c < "$dest" | tr -d ' ')
    [ "$size" = "$want" ] && return 0
  done
  echo "  size mismatch after $attempt tries: $object got $size want $want" >&2
  return 1
}

record() { # <file> <object> <rows>, appends one manifest entry
  python3 - "$1" "$2" >> "$3" <<'PY'
import gzip, hashlib, json, sys
path, object_name = sys.argv[1:]
data = open(path, "rb").read()
enc, raw = "identity", len(data)
if data[:2] == b"\x1f\x8b":
    enc, raw = "gzip", len(gzip.decompress(data))
print(json.dumps({
    "object": object_name,
    "stored": len(data),
    "raw": raw,
    "encoding": enc,
    "sha256": hashlib.sha256(data).hexdigest(),
}))
PY
}

# pick_reads <listing.json> <run prefix>
# The objects run.Build actually reads: six fixed ones, the largest extension
# result of each kind, and any bare junit.xml.
pick_reads() {
  python3 - "$1" "$2" <<'PY'
import json, sys
items = json.load(open(sys.argv[1]))["items"]
prefix = sys.argv[2] + "/"
by_name = {i["name"]: i for i in items}
fixed = ["prowjob.json", "finished.json", "artifacts/ci-operator-step-graph.json",
         "artifacts/junit_operator.xml", "build-log.txt", "artifacts/ci-operator-metrics.json"]
chosen = []
for f in fixed:
    if prefix + f in by_name:
        chosen.append(by_name[prefix + f])
for kind in ("extension_test_result_e2e__", "extension_test_result_monitors__"):
    cand = [i for i in items
            if kind in i["name"].split("/")[-1] and i["name"].endswith(".json")]
    cand.sort(key=lambda i: int(i["size"]))
    if cand:
        chosen.append(cand[-1])
bare = [i for i in items if i["name"].split("/")[-1] == "junit.xml"]
bare.sort(key=lambda i: int(i["size"]))
if bare:
    chosen.append(bare[-1])
for i in chosen:
    print("%s|%s" % (i["name"], i["size"]))
PY
}

for run in "${RUNS[@]}"; do
  IFS='|' read -r corpus name prefix <<< "$run"
  echo "== $corpus/$name"
  list_objects "$prefix/" "$TMP/$corpus-$name-listing.json"
  src="$TMP/$corpus-$name-listing.json"

  dest="$OUT/$corpus/jobs/$name"
  rm -rf "$dest"; mkdir -p "$dest/objects"
  rows="$TMP/$corpus-rows.jsonl"

  # Keep every name and size (inventory() needs them) but compress the listing.
  # mtime=0 keeps the file byte-identical between runs, so re-recording does not
  # produce a spurious diff.
  python3 - "$src" "$dest/listing.json.gz" <<'PY'
import gzip, json, sys
items = json.load(open(sys.argv[1]))["items"]
with open(sys.argv[2], "wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as f:
        f.write(json.dumps({"items": items}, separators=(",", ":")).encode())
PY

  pick_reads "$src" "$prefix" > "$TMP/$corpus-$name-reads.txt"
  while IFS='|' read -r object size; do
    if fetch_object "$object" "$dest/objects/$object" "$size"; then
      record "$dest/objects/$object" "$object" "$rows"
      echo "  ok $object"
    fi
  done < "$TMP/$corpus-$name-reads.txt"
done

# ---------- presubmit run index, for history ----------
for entry in "${INDEX_JOBS[@]}"; do
  IFS='|' read -r corpus job <<< "$entry"
  echo "== $corpus/health/$job"
  list_objects "pr-logs/directory/$job/" "$TMP/$corpus-health.json"
  hdest="$OUT/$corpus/health/$job"
  rm -rf "$hdest"; mkdir -p "$hdest/objects"
  python3 - "$TMP/$corpus-health.json" "$hdest/listing.json.gz" <<'PY'
import gzip, json, sys
items = json.load(open(sys.argv[1]))["items"]
with open(sys.argv[2], "wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as f:
        f.write(json.dumps({"items": items}, separators=(",", ":")).encode())
PY

  python3 -c 'import json,sys
for i in json.load(open(sys.argv[1]))["items"]: print(i["name"])' "$TMP/$corpus-health.json" \
  > "$TMP/$corpus-pointers.txt"
  kept=0; pruned=0
  while IFS= read -r object; do
    base="${object##*/}"
    # latest-build.txt holds a bare run id, not a gs:// pointer.
    [ "$base" = "latest-build.txt" ] && continue
    if fetch_object "$object" "$hdest/objects/$object"; then
      ptr=$(cat "$hdest/objects/$object")
      target="${ptr#gs://$BUCKET/}"
      if fetch_object "$target/finished.json" "$hdest/objects/$target/finished.json"; then
        kept=$((kept+1))
      else
        pruned=$((pruned+1))
      fi
    fi
  done < "$TMP/$corpus-pointers.txt"
  echo "  kept=$kept pruned=$pruned"
done

# ---------- one manifest per corpus ----------
python3 - "$OUT" "$TMP" <<'PY'
import glob, json, os, sys
out, tmp = sys.argv[1], sys.argv[2]
for rows in sorted(glob.glob(os.path.join(tmp, "*-rows.jsonl"))):
    corpus = os.path.basename(rows)[:-len("-rows.jsonl")]
    entries = [json.loads(l) for l in open(rows) if l.strip()]
    manifest = {
        "source": "https://github.com/rh-ecosystem-edge/recert/pull/2364"
                  if corpus == "recert-2364"
                  else "https://github.com/openshift/cert-manager-operator/pull/533",
        "captured": "2026-11-13",
        "note": "stored == GCS listing size; raw == decompressed/served size",
        "objects": entries,
    }
    path = os.path.join(out, corpus, "manifest.json")
    with open(path, "w") as f:
        json.dump(manifest, f, indent=2, sort_keys=True)
        f.write("\n")
    print("%s: %d bodies, %d stored bytes"
          % (corpus, len(entries), sum(e["stored"] for e in entries)))
PY

du -sh "$OUT"/*
