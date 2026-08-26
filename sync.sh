#!/usr/bin/env bash
set -euo pipefail

# --delete: without it rsync only ever adds and overwrites, so a file deleted
# here lingers on the host forever. That is not cosmetic - `go build ./cmd/...`
# compiles whatever is in the directory, so a stale file that no longer matches
# the rest of the package breaks the build on the host while it builds fine
# locally. Moving gh_stats.go to cmd/releases did exactly that: cmd/proxy kept
# calling a method that had moved with it.
#
# The deletion is scoped to the trees actually being transferred (cmd/ and the
# named files), so unrelated contents of the deploy root - cert-cache/,
# docker-compose.yml, anything else living there - are left alone.
rsync -avz --delete \
  "$(pwd)/cmd" \
  "$(pwd)/nix" \
  "$(pwd)/go.sum" \
  "$(pwd)/go.mod" \
  "$(pwd)/run.sh" \
  "$(pwd)/Dockerfile.nix" \
  "$(pwd)/Dockerfile.proxy" \
  "$(pwd)/Dockerfile.releases" \
  root@host1.cypis.ovh:/persist/debuginfod.pwndbg.re/
