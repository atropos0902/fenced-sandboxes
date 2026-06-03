#!/bin/sh
# scripts/try.sh 'shell command the agent runs'   starts one run and prints how it went
set -eu
cd "$(dirname "$0")/.."
. .dev/env
api() { curl -sS -H "Authorization: Bearer $API_TOKEN" "$@"; }
id=$(api 127.0.0.1:58090/runs -d "$(AGENT="$1" python3 -c 'import json,os;print(json.dumps({"repo":"demo/shop","title":"Try","agent":os.environ["AGENT"]}))')" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
while :; do
  s=$(api 127.0.0.1:58090/runs/$id | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])')
  case $s in queued|running) sleep 1 ;; *) break ;; esac
done
api 127.0.0.1:58090/runs/$id; api 127.0.0.1:58090/runs/$id/events | python3 -c '
import json,sys
for e in json.load(sys.stdin):
    print(e["epoch"], e["kind"], json.dumps(e["body"])[:300])'
