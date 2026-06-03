#!/bin/sh
# Runs the end-to-end tests twice: once as it ships, once with the fault that
# leaves a replaced sandbox alive. Needs Docker and Go.
set -eu
cd "$(dirname "$0")/.."
scripts/dev.sh up -d --build >/dev/null 2>&1
. .dev/env; export API_TOKEN
sleep 5
go test -tags e2e -count=1 -v -timeout 30m ./e2e "$@"
FAULT_LEAVE_OLD_SANDBOX=1 scripts/dev.sh up -d >/dev/null 2>&1
sleep 5
FAULT_LEAVE_OLD_SANDBOX=1 go test -tags e2e -count=1 -v -run WrittenOff -timeout 10m ./e2e
scripts/dev.sh up -d >/dev/null 2>&1
