#!/usr/bin/env bash
#
#   ./run.sh          rebuild and recreate the proxy only - the usual redeploy
#   ./run.sh --all    also ClickHouse, the releases service and the nix backend
#
set -euo pipefail

# The build comes first on purpose. Without `set -e` a failed build still ran the
# `docker rm -f` below, killing a working container and leaving nothing to
# replace it with.

clickhouse() {
  # 26.5 rather than 25.3 for the filesystem() table function, which lists a
  # directory tree as a table and can populate the source index without a
  # Go-side walk. Verified first: every ClickHouse-backed test in cmd/proxy and
  # cmd/releases passes against it - schema creation, the ALTER migrations, the
  # Tuple round-trip and all the /stats queries.
  #
  # NOTE: a ClickHouse upgrade is one-way. Once it rewrites metadata, the old
  # version will not start on that data directory, and ch_data is a named volume
  # holding real access logs and buildid_state. Snapshot it before running this.
  docker run -d \
    -p 127.0.0.1:9000:9000 \
    --name clickhouse \
    --restart unless-stopped \
    --ulimit nofile=262144:262144 \
    --cap-add=SYS_NICE --cap-add=NET_ADMIN --cap-add=IPC_LOCK \
    -v "ch_data:/var/lib/clickhouse/" \
    -v "ch_logs:/var/log/clickhouse-server/" \
    -it clickhouse/clickhouse-server:26.5-alpine || true
}

proxy() {
  docker build -f Dockerfile.proxy -t "pwndbg-debuginfod-proxy" .
  docker rm -f "pwndbg-debuginfod-proxy" 2>/dev/null || true
  docker run -d \
    --name "pwndbg-debuginfod-proxy" \
    --restart unless-stopped \
    --network host \
    -e CACHE_PATH=/var/lib/pwndbg-debuginfod-cache \
    -v "pwndbg_debuginfod_cache:/var/lib/pwndbg-debuginfod-cache/" \
    "pwndbg-debuginfod-proxy"
}

releases() {
  docker build -f Dockerfile.releases -t "pwndbg-debuginfod-releases" .
  docker rm -f "pwndbg-debuginfod-releases" 2>/dev/null || true
  docker run -d \
    --name "pwndbg-debuginfod-releases" \
    --restart unless-stopped \
    --network host \
    "pwndbg-debuginfod-releases"
}

nix() {
  docker build -f Dockerfile.nix -t "pwndbg-debuginfod-nix2" .
  docker rm -f "pwndbg-debuginfod-nix2" 2>/dev/null || true
  # --cap-add SYS_ADMIN: the service mounts erofs images. The host kernel has
  # CONFIG_EROFS_FS_BACKED_BY_FILE=y, so no loop device is involved and this does
  # NOT need --privileged or /dev/loop-control.
  #
  # erofs is a module there (CONFIG_EROFS_FS=m) and a container cannot autoload
  # one, so the host must have it loaded already:
  #   echo erofs > /etc/modules-load.d/erofs.conf && modprobe erofs
  #
  # IMAGE_PATH/ENTRY_MOUNT_PATH point into the volume; without them the defaults
  # are relative to WORKDIR and the volume would hold nothing. Mount points stay
  # inside the container namespace and are rebuilt on demand - only the images
  # are worth keeping.
  docker run -d \
    --name "pwndbg-debuginfod-nix2" \
    --restart unless-stopped \
    --network host \
    --cap-add SYS_ADMIN \
    -e IMAGE_PATH=/var/lib/cache/images \
    -e ENTRY_MOUNT_PATH=/var/lib/cache/entry \
    -v "nix_cache:/var/lib/cache" \
    "pwndbg-debuginfod-nix2"
}

case "${1:-}" in
  "")
    proxy
    ;;
  --releases)
    releases
    ;;
  --nix)
    nix
    ;;
  --all)
    clickhouse
    proxy
    releases
    nix
    ;;
  -h | --help)
    sed -n '2,5p' "$0"
    ;;
  *)
    echo "unknown option: $1" >&2
    sed -n '2,5p' "$0" >&2
    exit 2
    ;;
esac
