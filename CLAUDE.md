# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Debuginfod infrastructure for [pwndbg](https://github.com/pwndbg/pwndbg), serving debug symbols and
sources at `debuginfod.pwndbg.re`. Single Go module (`pwndbg-debuginfod`, go 1.24) with several
`cmd/` binaries plus a shared `nix/` package. ClickHouse stores all state and telemetry.

## Repository state (read this first)

Not everything in `cmd/` builds. `go build ./...` currently fails:

- **`cmd/proxy`** — builds, deployed, the real production service.
- **`cmd/releases`** — builds, deployed, has tests. Serves `releases.pwndbg.re` on
  `127.0.0.1:8033`: the GitHub release redirect, the hourly download-stats worker, its own access
  log and a `/stats` page. It owns `github_download_stats` and `releases_access_log` and creates
  both itself.
- **`cmd/nix-nar-old`** — builds, deployed. Despite the name, this is what actually runs in
  production as the nix backend (see `Dockerfile.nix`).
- **`cmd/nix-debuginfod`** — all three endpoints (`debuginfo`, `executable`, `source`) work end to
  end in tests; **not deployed**, and not yet fit to be. `store.go` fetches a NAR, builds an erofs
  image, mounts it at the canonical store path and follows symlinks across images lazily. Still
  missing before it could replace `cmd/nix-nar-old`: eviction of images and mounts (nothing is ever
  unmounted or deleted), reconciliation of mounts orphaned by `docker rm -f`, and the deployment
  work (port 8032, `Dockerfile.nix`, `sync.sh` does not send `nix/`). Eviction is now a
  prerequisite rather than hygiene: a source request may name any store path, so a client can steer
  what gets downloaded.
- Verified end to end against the real binary cache (glibc,
  `8ae0b698f2d4e727f569f64bb166e08ae30bd077`): `debuginfo` and `executable` both come back as ELF
  files whose `.note.gnu.build-id` matches the request, and `source` serves real glibc sources.
  `Dockerfile.nix` needs `COPY nix/ ./nix` and `erofs-utils` (a **runtime** dependency —
  `NewNixDebuginfo` `log.Fatal`s without `mkfs.erofs`), and `run.sh` needs `--cap-add SYS_ADMIN`.
  On the host that is enough because file-backed mounts need no loop device; on a kernel without
  `CONFIG_EROFS_FS_BACKED_BY_FILE` the loop fallback also needs the loop devices, so a local smoke
  test wants `--privileged`.
- `worker.go` is **deleted**. Its pool solved bounded concurrency but not the problem that actually
  bites — two requests for the same cold build ID — which `singleflight` in `store.go` does. The
  one thing it did that singleflight does not, capping how many *distinct* paths build at once,
  came back as a semaphore in `buildOnce`: a bound, not a queue, because a queue would also have to
  answer what happens when it fills.
- **`cmd/indexer`** — builds and has tests; **nothing serves from its output yet**. It walks
  `astral-sh/python-build-standalone` releases and records which asset holds the debug info for a
  given build ID, into `pbs_buildids` + `pbs_assets`. It exists because uv downloads the
  **`install_only_stripped`** tarballs: those binaries keep `.note.gnu.build-id` — stripping does
  not touch it — but carry no DWARF, and no distro debuginfod knows the IDs, since these are
  astral's builds. The `install_only` asset of the same release has the same binaries unstripped,
  at the same member paths, with the same IDs. Verified on 3.14.7+20260825: `python3.14` is
  `64c83ec8…d750` in both, 32 MB stripped against 116 MB with `.debug_info`, and all 9 build IDs
  agree across the two assets (`TestStrippedAndFullShareBuildIDs`, needs local tarballs).
  It used to be a Debian `.deb` scratchpad; that direction was dropped because `debuginfod.debian.net`
  already covers Debian debuginfo, and the one gap it does not cover — `/source/*` — is in `.dsc`
  and `.orig.tar.xz`, not in a `.deb`.
- **`cmd/deb-debuginfod`** + **`cmd/deb-unpack`** — build, have tests, verified end to end; **not
  deployed**. They serve `/buildid/<id>/source/*` for Debian build IDs and nothing else, because
  `debuginfod.debian.net` already does debuginfo and executable well but returns 404 for every
  source path — which is why `cmd/proxy` carries `SourceAvailable: 0` for debian and answers 501.
  Measured on production: `/source` succeeds for 1.1% of requests, and 1,843 of 3,641 in a week
  were that 501.
- **`cmd/debug-nar`** — builds; a personal scratch harness for poking at erofs images, with most of
  `main` commented out and hardcoded `/root/.cache/...` paths.

`cmd/proxy`, `cmd/releases`, `cmd/nix-nar-old` and `cmd/indexer` have tests; the other packages have
none. Every package compiles, so `go test ./...` works — but prefer naming the suites, since the
whole-module run buys nothing:

```bash
go test ./cmd/proxy ./cmd/releases ./cmd/nix-nar-old ./cmd/indexer -race   # ~5s, no external deps
```

`cmd/indexer`'s two heaviest tests are skipped unless pointed at local tarballs
(`PBS_TEST_TARBALL`, `PBS_TEST_TARBALL_STRIPPED`) — the synthetic archives cannot catch a change in
how the release pipeline lays assets out.

**A build-ID note is not near the front of the file.** `BuildIDFromELF` streams forward and takes
the stream length from the tar entry header, because python-build-standalone links its interpreters
so `PT_NOTE` lands at the *end*: `python3.10` is 46 MB with its note segment at `0x2c3d0a8`, 66 kB
before EOF. The first version capped the forward scan at 16 MiB on the theory that notes follow the
program headers, and so silently dropped exactly the build ID a user is most likely to want — the
interpreter's own — while still returning the nine ids of the shared libraries around it, which is
what made it look like it worked. `TestBuildIDFromELFFindsNoteAtEndOfFile` pins it. Reading to the
end of an entry costs nothing extra anyway: `tar.Reader` has to stream past it to reach the next
header.

`cmd/nix-nar-old`'s tests exist for one reason: its handler passes the `ResponseWriter`
straight to `Get`, so a failure *after* the first byte cannot change the status. It used to
call `http.Error` anyway, which appended `Internal Server Error` to the partial debuginfo —
a 200 with a self-consistent length and a cleanly terminated stream, which the proxy then
cached and served on. It now `panic(http.ErrAbortHandler)`s instead, and
`TestErrorAfterHeadersBreaksConnection` pins that. `globalNix` is an interface variable
purely so that test can substitute a getter that writes and then fails.

ClickHouse runs **26.5** (`run.sh`), raised from 25.3 for the `filesystem()` table function —
a recursive directory listing as a table, where `path` is the *parent* directory, `name` the entry
and `type` its kind. It exists only from 26.5. Note that a ClickHouse upgrade is one-way: after it
rewrites metadata the old version will not start on that data directory.

ClickHouse-backed tests (schema creation, `ALTER` migrations, Tuple round-trip through
`AsyncInsert`, every `/stats` query in `cmd/releases`) are skipped unless a DSN is provided.
They **`DROP TABLE`** under the production names, so `testDB` refuses any DSN whose port is
`9000` — that is production, and it is reachable from a dev machine. Use another port. **Run them whenever you touch a table
definition** — `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table, so a new column
without a matching `ALTER … ADD COLUMN IF NOT EXISTS` compiles, passes every offline test, and
then fails on the deployed instance only:

```bash
docker run -d --name ch -p 19000:9000 \
  -e CLICKHOUSE_USER=cypis -e CLICKHOUSE_PASSWORD=cypis \
  clickhouse/clickhouse-server:26.5-alpine
TEST_CLICKHOUSE_DSN='clickhouse://127.0.0.1:19000?username=cypis&password=cypis' \
  go test ./cmd/proxy ./cmd/releases -run TestDB -v
```

Concurrency is the main risk area in this codebase (request coalescing, shared cache state,
fan-out streaming), so **always run with `-race`**.

Many tests exist specifically to pin down bugs that were already fixed once. They look
unremarkable and are the wrong ones to delete during a cleanup — each is the sole guard on a
decision that took a while to get right:

| where | pins |
|---|---|
| `TestMiddlewareNoSuperfluousWriteHeaderAfterCommit` | a committed response is aborted, not re-written |
| `TestTruncatedResponseIsDetectableByClient` (`truncation_test.go`) | a short body must break the connection, not look complete |
| `TestResolvedHeadersMatchWinningHost` | headers come from the host that actually answered |
| `TestConcurrentResolutionsAreCoalesced` | N parallel lookups make one upstream fan-out |
| `TestFileCacheSlowClientDoesNotBlockOthers`, `…LateJoinerGetsWholeFile` | followers stream at their own pace from a growing file |
| all of `fixes_test.go` | invalid `.meta` status, the reader retry branch, non-blocking `ErrCacheBusy`, blob rollback on a failed `.meta` |
| `TestDownloadDoesNotLogUnderEntryLock` (`teardown_test.go`) | no logging under `e.mu` — a blocked stderr must not stall the process |
| `TestEvictOnceFailsOnUnreadableRoot`, `…WarnsAboutSkippedEntries` | eviction reports what it could not read instead of reporting an empty cache |
| `TestErrorAfterHeadersBreaksConnection` (`cmd/nix-nar-old`) | a failure after the first byte aborts the connection instead of appending `Internal Server Error` to the payload |
| `TestOpenCachedKeepsFilesOnCorruptEntry` | a bad entry is logged, **never deleted** — a transient errno must not destroy an archived artifact |
| `TestEvictOnceSweepsAbandonedTempFiles` | orphaned `.tmp-*` are reclaimed, but only past `cacheFetchTimeout` — a younger one may belong to a live download |
| `TestOpenCachedTreatsMissingMetaAsPlainMiss` | the `os.IsNotExist` carve-out; folding it away would delete blobs during the normal publish window |
| `TestDBCacheStatsMigrationAddsApparentBytes` (needs a DSN) | adding a column to a `CREATE TABLE IF NOT EXISTS` block is invisible to deployed instances — this one broke production once |
| `TestCatchAllRedirectIsNotLogged` (`cmd/releases`) | the project-page redirect stays out of `releases_access_log`, so crawler traffic cannot enter `/stats` |
| `TestRenderEscapesAttackerControlledLabels` (`cmd/releases`) | a filename is client-supplied, stored verbatim and rendered as a bar label — the escaping is the only thing between a crafted request and stored XSS |
| `TestClientIPTrustsHeaderOnlyFromLoopback` (`cmd/releases`) | `CF-Connecting-IP` is believed only from the tunnel, never from a routable peer |

**Every outbound HTTP request goes through `useragent.Client`.** The header is set in a
RoundTripper, not by callers, because setting it at call sites has failed twice: the proxy's
resolution probes went out as `Go-http-client/2.0` until someone noticed, and `nix/` used
`http.DefaultClient` with no User-Agent at all for every request to `cache.nixos.org`. A component
name (`nix`, `deb`, `deb-unpack`) tells an upstream operator which of our services is calling.
`cmd/proxy` keeps its own equivalent, `newUpstreamRequest`, which both `Fetch` and `Probe` go
through for the same reason.

**`buildcheck/` tests how the repository is assembled, not what it does.** It asks the toolchain
what each Dockerfile's binary imports and fails if the Dockerfile does not `COPY` that package, and
checks `sync.sh` sends it. That failure is remote-only — the image builds locally from the module
cache and its `COPY` fails on the host — and it has happened twice, with `srcindex/` and
`useragent/`. Both lists are explicit rather than globs, so adding a top-level package is a step
that is easy to forget; this is what remembers.

Four small interfaces exist purely so tests can avoid ClickHouse: `accessLogger`
(`context.go`), `stateStore` (`finder.go`), `statsSource` (`stats.go`) and `cacheStatStore`
(`cachestat.go`). `*dbSrv` satisfies all four implicitly.

**`cmd/proxy` is English-only** — every comment and SQL column comment there was translated in
one pass, so write new ones in English and do not reintroduce Polish. The other packages
(`cmd/nix-nar-old`, `cmd/nix-debuginfod`, `nix/`) still mix the two; match whatever the surrounding
file uses rather than translating opportunistically.

## Build and run

```bash
go build -o bin/proxy ./cmd/proxy        # or: go run ./cmd/proxy
go build -o bin/nix   ./cmd/nix-nar-old

go build ./cmd/proxy ./cmd/nix-nar-old   # build only what compiles
```

`docker-compose up -d` starts **Grafana only** (host networking, port 3000, admin/admin). The
ClickHouse service is commented out in `docker-compose.yml`; `run.sh` starts it as a standalone
container instead (`clickhouse/clickhouse-server:26.5-alpine`, bound to `127.0.0.1:9000`).

Local ClickHouse needs a user matching the default DSN:

```sql
CREATE USER cypis IDENTIFIED WITH plaintext_password BY 'cypis';
GRANT ALL ON default.* TO cypis;
```

Tables are created idempotently by `db.Init()` on service startup — no migration tooling.

## Deployment

**Ingress, edge caching and the environment invariants the design leans on are described in
[INFRA.md](INFRA.md)** — none of it lives in
this repository. Two facts from there change how this file reads: a `cloudflared` daemon on the
host terminates and forwards to `127.0.0.1:8031`, which makes the whole autocert path in
`main.go` dead code (`LISTEN_PORT` is never 443); and the Cloudflare edge cache is a dashboard
Cache Rule holding 200s for a year and 404/501 for two hours, with every other status code
uncached — which means `/stats` counts
origin load rather than client demand, and the sub-2h rungs of the negative-result backoff are
invisible to most clients.

Manual, two steps, no CI:

```bash
./sync.sh          # rsync cmd/ go.mod go.sum run.sh Dockerfile.* -> root@host1.cypis.ovh:/persist/debuginfod.pwndbg.re/
./run.sh           # on the host: rebuild and recreate the proxy only
./run.sh --all     # also ClickHouse, cmd/releases and the nix backend
```

`run.sh --all` also brings up the ClickHouse container. `Dockerfile.proxy` and `Dockerfile.releases` are `COPY cmd/ ./cmd` only; `Dockerfile.nix` also
does `COPY nix/ ./nix`, because `cmd/nix-debuginfod` imports that package, and installs
`erofs-utils` — a **runtime** dependency, since `nix.NewNixDebuginfo` `log.Fatal`s without
`mkfs.erofs`.

## Configuration

Env vars via `github.com/caarlos0/env/v11` (`config.go` in each cmd). Note the defaults differ from
production values:

| Var | Default | Notes |
|---|---|---|
| `CLICKHOUSE_DSN` | `clickhouse://127.0.0.1:9000?username=cypis&password=cypis` | both services |
| `LISTEN_PORT` | `8031` | proxy; **TLS/autocert only engages when this is exactly 443** |
| `LISTEN_IP` | `127.0.0.1` | proxy |
| `DOMAIN` | `debuginfod.pwndbg.re,releases.pwndbg.re` | comma-separated autocert whitelist |
| `LETSENCRYPT_EMAIL` | `patryk.sondej@gmail.com` | |
| `CERT_CACHE_PATH` | `./cert-cache` | |
| `LOG_LEVEL` | `info` | |
| `CACHE_ENABLED` | `true` | the only kill switch for the on-disk cache |
| `CACHE_PATH` | `./cache` | **`CACHE_PATH=` does not disable it** — env/v11 falls back to `envDefault` on an empty value, so use `CACHE_ENABLED=false` |
| `CACHE_MAX_BYTES` | `53687091200` | 50 GiB; LRU by mtime, swept every 10 min |
| `STATS_ENABLED` | `true` | drops the `/stats` route when false |
| `STATS_DAYS` | `360` | longest `/stats` window; the 7/30/180 d views are sliced from the same data |
| `STATS_INTERVAL` | `1h` | background rebuild period; the handler never queries ClickHouse |
| `CACHE_STATS_INTERVAL` | `10m` | how often `CACHE_PATH` is measured into `cache_stats` |
| `GH_RANGES_ENABLED` | `true` | refreshes `github_ip_ranges` from `api.github.com/meta`; **false does not remove the `/stats` filter**, it only stops updating what the filter reads |
| `GH_RANGES_INTERVAL` | `24h` | the ranges are Azure allocations that move on the scale of weeks |
| `GH_RANGES_SERVICES` | `actions` | comma-separated `meta` keys; `actions_macos` is separate and not included |

`cmd/releases` reads `CLICKHOUSE_DSN`, `LOG_LEVEL` and the same `STATS_ENABLED` / `STATS_DAYS`
(360) / `STATS_INTERVAL` (1h) trio; it hardcodes `127.0.0.1:8033`.

The cache is **on by default**. `run.sh` overrides the default with
`CACHE_PATH=/var/lib/pwndbg-debuginfod-cache` on a named volume, so in production it **survives**
`docker rm -f`. It does not set `CACHE_MAX_BYTES`, so the 50 GiB default applies whatever the
volume's real size is. Edge caching is configured in the Cloudflare GUI (a Cache Rule with a
1-year Edge TTL), not in this repo — nothing here would tell you that, and nothing here
reproduces it.

`cmd/nix-nar-old` takes no config; it hardcodes `127.0.0.1:8032`.

## Architecture

### Proxy (`cmd/proxy`) — the core of the system

Request path: client → `AccessLogMiddleware` → `proxyRequest` → `finder.FindByBuildID` →
`fileCache.Serve` (for `debuginfo`/`executable`) → stream from disk or from the winning upstream.

Note the ordering: **resolution happens before the cache is consulted**, so a `HIT` still costs a
`FindByBuildID` — and if that fails (ClickHouse unreachable, or the recorded host is no longer in
the `servers` map) the request 404s or 500s even though the bytes are complete on disk.
`/source/*` never reaches the cache at all.

**Resolution (`finder.go`)** is the piece worth understanding. `FindByBuildID` never queries a build
ID against all upstreams on the hot path more than once:

1. `GetState` checks an in-process `expirable.LRU` (10k entries, 24h TTL), then `buildid_state` in
   ClickHouse.
2. On a recorded success, the stored host name is looked up in the static `servers` map and returned
   immediately.
3. Otherwise `tryAllServers` fires a `HEAD`-like GET of `/buildid/<id>/debuginfo` at **every**
   upstream concurrently under a 5s `maxResolutionTimeout`, takes the first 200, and cancels the
   rest. If all fail, the channel closes and the result is `ErrDebuginfoNotFound`.
4. The outcome is written back to cache + `buildid_state` via a `defer`.

**Negative-result backoff** is keyed on `state.Counter`, not a timestamp schedule: retry is
suppressed for 30m after the 1st failure, 1h after the 2nd, 2h after the 3rd, then 24h. Past
`counter > 30` the build ID is refused permanently without any upstream traffic. Changing these
constants changes the load profile against every upstream at once.

**Source handling**: `Server.SourceAvailable` marks upstreams that serve `/source/*`. Debian and
Ubuntu are 0. For the `source` endpoint the finder converts both "not found" and
"host has no sources" into `ErrSourceNotImplemented` → HTTP 501, deliberately so results stay
cacheable per build ID.

**Upstreams** are a hardcoded map in `NewDebugInfoFinder`. Current set: systemtap, opensuse, fedora,
archlinux, artixlinux, cachyos, centos, debian, ubuntu, and `nix` at `http://127.0.0.1:8032`.
`elfutils` (bugged) and `alpine` (offline) are commented out — keep them commented rather than
deleting, they document why. A host name persisted in `buildid_state` that no longer exists in this
map degrades to a 404 with a warning log.

**Client IP** (`cfip.go`): the proxy sits behind Cloudflare. `getRealIP` trusts `CF-Connecting-IP`
only when `RemoteAddr` falls inside a prefix fetched from the Cloudflare IPs API (refreshed via a
background worker, lazily initialized through a `sync.Once` singleton).

**CI classification** (`ghranges.go`): `/stats` is meant to describe people, and a workflow that
installs pwndbg on every push is not one. Measured on production, **89% of all traffic is GitHub
Actions** — 12.05M of 13.48M rows — so this is not a rounding correction. A daily worker fetches
`api.github.com/meta`, keeps the `actions` prefixes in an in-process `ipTagSet`, and
`AccessLogMiddleware` tags each request as it is logged. `country` is filled the same way, from
`CF-IPCountry`, trusted only from a Cloudflare peer exactly like `CF-Connecting-IP`.

**The row's own tags are the only input to the filter** — `excludeCI` is just
`NOT has(tags, 'github_actions')`. There used to be a second mechanism, an `ip_trie` dictionary in
ClickHouse consulted for rows that predated tagging, and it was **removed** once
`scripts/backfill_tags.py` had classified every one of them. Do not reintroduce it as a general
safety net: matching an old row against today's ranges reattributes traffic every time GitHub hands
a prefix back to Azure, which is exactly why the decision is recorded per row rather than
recomputed.

`tags` therefore has three values and the third is the point:

| value | meaning |
|---|---|
| `github_actions` | matched the range list as it stood when the request arrived |
| `direct` | checked against that list and did not match |
| `unclassified` | could not be checked — the list had never loaded |

`unclassified` exists because the dictionary is gone and nothing can reconstruct the answer later.
It is a tag rather than an empty array so the gap is *countable*: one query says how many rows are
affected and `scripts/backfill_tags.py` repairs precisely those. An empty array would be
indistinguishable from a pre-tagging row, and after the backfill there are none of those left.
`/stats` **counts** `unclassified` rows rather than dropping them — the conservative direction, so
a gap shows up as ordinary traffic instead of silently vanishing.

The window is normally milliseconds: `Worker` refreshes before the first tick and retries every
`retryUntilLoaded` (15 s) until it has succeeded once, rather than waiting out the 24 h interval —
otherwise an unreachable GitHub at startup would cost a day of unclassified rows.

`ipTagSet` buckets prefixes by mask length instead of scanning them, because `cfip.go`'s linear
scan is right for thirty Cloudflare ranges and wrong for seven thousand on the hot path.

Filtering only covers `access_log`. `collectProbes` reads `resolve_logs`, which has no `remote_ip`,
so upstream probe counts still include probes triggered by CI requests.

**Error → status mapping** happens in `AccessLogMiddleware`, not in handlers: handlers return
`error`, the middleware maps `ErrSourceNotImplemented`→501, `ErrDebuginfoNotFound`→404, anything
else→500, then writes the access log with a fresh `context.Background()` (the request context is
usually already cancelled by then).

**Cache measurement** (`cachestat.go`, `fsspace_unix.go`): a worker walks `CACHE_PATH` every
`CACHE_STATS_INTERVAL` and inserts one `cache_stats` row. Deliberately a separate walk rather
than piggybacking on `evictOnce` — that function has three tests pinning its error behaviour and
returns early when `CACHE_MAX_BYTES <= 0`, which is precisely the configuration where usage
numbers matter most. Size comes from `st_blocks`, not `info.Size()`: `CACHE_PATH` sits on **btrfs**, where
compression makes a file occupy less than its length and block rounding makes small files occupy
more, so a sum of lengths is not occupancy in either direction. Both numbers are stored
(`bytes` and `apparent_bytes`) and the page shows the ratio when they diverge. Note this differs
from `evictOnce`, which still compares `CACHE_MAX_BYTES` against summed `info.Size()` — on a
compressed volume the budget therefore bites earlier than the disk requires. Free space uses
`statfs` **Bavail**, not Bfree, so the root reserve is not counted as available; on btrfs that
figure is an estimate, since ENOSPC can arrive through exhausted metadata chunks while free
space still looks ample. A failed scan skips the insert entirely: a row of zeros would look like an
empty cache forever, while a missing row is only a gap in the chart. `fsspace_unix.go` carries a
`//go:build unix` tag with no fallback — the package already cannot build elsewhere, because
`filecache.go` builds its fan-out on `syscall.Dup`.

**Stats page** (`stats.go` collects, `stats_render.go` draws): `GET /stats` serves a
self-contained HTML page of 180-day charts. Five aggregate queries scan the whole window, so
they run on a `STATS_INTERVAL` timer in a goroutine and the finished bytes sit in an
`atomic.Pointer`; the handler only writes them, never touching the DB. Four views (7/30/180/360 d)
come from **one** collection — `sliceSnapshot` cuts the tail of the daily series and recomputes
everything derived from them (host ordering, probe totals, shared throughput scale), so adding a
view costs render time, not ClickHouse queries. `?days=` accepts only values in that list.
The moving-average window scales with the view (`smoothWindowFor`): 7 d renders raw daily values,
because a 7-day average over seven days is one flat line. A failed rebuild keeps
the previous page — a ClickHouse blip should give stale numbers, not a 503. Deliberately **not**
wrapped in `AccessLogMiddleware`: the page renders `access_log`, so logging its own hits would
fold page views into the numbers on display. `statsSource` is the third test-only interface,
alongside `accessLogger` and `stateStore`, and exists so the render tests need no ClickHouse.
Three drawing decisions are load-bearing and have tests: `aborted` and `5xx` are under 0.02% of
traffic so they also get per-day ticks below the axis (`TestTrafficPanelMarksRareCategories`);
a series with no samples draws nothing rather than a band lying on the axis, which would
read as a measured zero (`TestBandPanelSkipsEmptySeries`); and the country panel's folded tail is
a line, never a bar (`TestCountryTailIsNotDrawnAsABar`).

**Offline upstreams** (`collectProbeRecent`, `offlineHosts`): `tryAllServers` probes *every*
configured upstream on every cold build ID, so a host still in the `servers` map cannot go a day
without rows in `resolve_logs`. Worth knowing *why* that holds, because the obvious reading of
`tryAllServers` says otherwise: `ResolveLog` is called once, inside `case res, ok := <-ch`, and it
copies `entries` at the instant a winner arrives — probes still in flight append afterwards and are
dropped (there is a `TODO: move this to the background` on that line). On a resolution that fails
everywhere, though, `close(ch)` happens only after `errCounter.Add(1) == maxErrors`, and the append
precedes that increment, so every host is recorded. Since ~94% of traffic resolves to nothing, that
path alone gives complete daily coverage. The dropped entries do under-count probes for a
consistently slow host, which is a `resolve_logs` accuracy problem in its own right.

Zero probes in the last 24 h therefore means the host was removed from
that map — `elfutils` and `alpine` are commented out there — and an `offline` badge goes beside its name. It is still charted, because it still has history.

**The badge belongs to every panel that names a host** — traffic, bytes, probes and throughput, all
four. `isOffline` is a predicate on the snapshot rather than a precomputed set for exactly that
reason: the first version marked the probes panel only, and the same upstream then read as live in
one section and dead in the next. `offlineBadge` is likewise one piece of markup shared by all four
call sites. `TestProbePanelMarksUnprobedHostsOffline` walks each section by heading and fails if any
one of them omits it — verified by reverting each panel in turn.

**Two guards, both load-bearing.** `isOffline` returns false for everything when
`ProbeRecentTotal` is zero: nobody being probed is the signature of a night with no cold build IDs,
not of twelve simultaneous outages, and a page announcing the latter would be worse than one saying
nothing (`TestProbePanelSaysNothingWhenNobodyWasProbed`). And it returns false for `unresolvedLabel`,
which is not a host but the absence of one — it is never probed, so without the carve-out it would
be reported offline on every page load. Note the badge does **not** catch
`Down: true` — `tryAllServers` ignores that flag, so such a host is still probed and still looks
live here. The query is a second, cheap pass rather than a `countIf` in the main probe query:
`resolve_logs` is `ORDER BY (timestamp, buildid)`, so a one-day predicate reads one day of parts
instead of the whole 360-day window.

**Country panel** (`collectCountries`, `renderCountries`): a ranked bar list of where requests come
from, with ~200 categories and a long tail — a comparison of magnitudes, not a shape over time.
Points worth keeping:

- Collected **per country per day**, not as one total each, because four views are sliced from one
  collection and a total cannot be cut down to seven days. `sliceSnapshot` re-sorts afterwards: the
  leader over 360 days need not lead over 7 (`TestSliceSnapshotRecomputesCountryOrder`).
- Two numbers per row. Requests sum across days; **distinct clients do not**, so the second column
  is the *peak* daily figure rather than a sum — a client active on thirty days would otherwise
  count as thirty. It earns its place: production has countries sending tens of thousands of
  requests from ten addresses.
- The tail past `countryRows` (12) is a summary line. Its total routinely exceeds the leading
  single country, so as a bar it rendered at 257% and `overflow:hidden` clipped it to full width,
  making the residual look like the largest single origin. Only rendering the page showed this.
- `country = ''` is dropped rather than shown as "unknown": after the backfill the only addresses
  without one are `127.0.0.1` and the Docker bridge — this host talking to itself.
- The panel carries the same endpoint and CI filters as the ones beside it
  (`TestDBCollectCountriesFiltersLikeTheOtherPanels`). Without the CI filter it would be a map of
  Azure regions rather than of users.
- `flagFor` derives the flag emoji arithmetically and refuses anything that is not two ASCII
  letters, including Cloudflare's `XX` and `T1`. `countryLabel` exists because there are two call
  sites and the first version escaped only one of them — `country` comes from `CF-IPCountry`, so
  that was a live injection point (`TestCountryPanelEscapesAttackerControlledLabels`).

### File cache (`filecache.go`)

One leader downloads into `<blob>.tmp-*` while N followers stream the growing file through
dup'd descriptors and `ReadAt`, waiting on a `sync.Cond` when they catch up. On completion the
temp file is `rename`d to a sha256-named blob with a sibling `.meta`. Deliberate decisions worth
knowing before editing:

- **One representation.** `Accept-Encoding: gzip` is forced upstream and the stored bytes go to
  every client verbatim — no negotiation, no decompression, so no `Vary` is needed. This assumes
  Cloudflare fronts the origin; see README.
- **A bad entry is logged, never deleted.** A transient errno says nothing about the file, and
  `os.Rename` overwrites the entry on the next fetch, so deletion buys nothing and can destroy
  an artifact whose upstream has since dropped it. `TestOpenCachedKeepsFilesOnCorruptEntry`.
- **`e.size` is a promise.** It is published only after every buffer stage has been flushed, and
  the counting writer sits *below* the `bufio` buffer so it can never run ahead of the file. Get
  this wrong and followers read past EOF.
- **Nothing is logged under `e.mu`.** `Serve` holds the global `c.mu` while taking `e.mu`, so a
  blocking write to a stalled stderr pipe would freeze every cacheable request in the process.

### Nix backend (`cmd/nix-nar-old`, deployed)

Serves `:8032`. Fetches `https://cache.nixos.org/debuginfo/<buildid>` for `{archive, member}`,
downloads the NAR (transparently xz-decompressed by sniffing the `nix-archive-1` magic), caches it
whole under `os.UserCacheDir()/nix-debuginfod/`, then linearly scans the NAR for `member` and streams
it out. `/source/*` returns 501.

### Nix rewrite (`nix/` package + `cmd/nix-debuginfod`, WIP)

**The read path does not exist.** `nix/` is write-only: it turns a NAR into an erofs image and
stops. Nothing opens one — `github.com/erofs/go-erofs` is imported solely by the `cmd/debug-nar`
scratch harness, and that library has no symlink support at all (no `S_IFLNK`, no readlink), so
the plan is to mount images rather than read them in userspace.

The design that follows from that, verified against a real kernel rather than inferred:

- One image **per store path**, mounted at the canonical `/nix/store/<hash>-<name>`. Absolute
  symlinks between store paths then resolve in the kernel, so nothing has to walk them in Go.
- Resolution is lazy: `os.Open`, and on `ENOENT` walk the path with `lstat` to find the first
  symlink naming an unmounted store path, fetch and mount that one, retry. Bounded by iterations
  rather than link depth, plus a cycle guard. `References` from the narinfo is **not** needed — a
  symlink target already carries the store hash. The walk must follow *through* links that already
  resolve and carry the unresolved tail across them; stopping at the first link that points
  somewhere already mounted breaks every chain longer than one hop.
- **The `-debug` NAR names everything else that build ID has**, as three symlinks sitting beside
  each `.debug` file. Verified against `cache.nixos.org` (glibc, 389 build IDs, 1167 links):

  ```
  lib/debug/.build-id/8a/e0b6….debug            a real file, the debug info
  lib/debug/.build-id/8a/e0b6….executable    -> /nix/store/<hash>-glibc-2.42-61/lib/gconv/IBM1142.so
  lib/debug/.build-id/8a/e0b6….source        -> /nix/store/<hash>-glibc-2.42.tar.xz
  lib/debug/.build-id/8a/e0b6….sourceoverlay -> ../../../../src/overlay
  ```

  So nothing has to be indexed, parsed out of DWARF, or looked up in narinfo `References` — the
  answer is a symlink, and the siblings are the member with its extension swapped. This is what
  is why `nix_source_files` and its lookup code were **deleted**: the index existed to rebuild,
  in a table, information the archive already states outright. It had also never run — the schema
  declared `buildid UInt64` while the Go side passed a 40-character hex string, so both the insert
  and the lookup would have failed on type.
- **`debuginfo` never needs lazy resolution** — its member is a real file in that same NAR. `Open`
  returns a hop count and `GetDebuginfo` warns when it is non-zero, because a silent extra fetch
  would hide a broken assumption instead of surfacing it. `executable` is the endpoint that does
  need it, and it works.
- **`source` reads two trees, and the order is load-bearing.** `.sourceoverlay` (inside the
  `-debug` NAR, at `/src/overlay/<pkg>/`) holds what the build actually compiled — patches applied,
  `configure` output, generated headers — and is tried **first**. `.source` holds the pristine
  upstream and is the fallback. Serving pristine where an overlay exists returns source that does
  not match the binary.
- `.source` is either an archive or a directory and the store does not distinguish them:
  `NarUnpackerAsTarball` branches on the NAR root type, so a `nar.TypeRegular` root goes to
  `unpackNarFileToTarball` (decompressed by `detectCompression` on the store path's *name*) and a
  directory is copied straight through. Either way a tree gets mounted.
- **A source request can name a store path directly** (`/nix/store/<hash>-dep/include/stdio.h`),
  which happens for a dependency's headers or anything compiled straight out of the store. That
  file is in neither the overlay nor `.source`, so it needs its own NAR. This branch runs **first**
  and does no suffix matching: the request is an exact reference, and falling through to the
  heuristic could match an unrelated `include/stdio.h` in this package's overlay and serve the
  wrong file silently. Because the path is client-supplied and each accepted one costs a narinfo
  fetch, `storePathOf` validates the hash as exactly 32 characters of nix base32
  (`0123456789abcdfghijklmnpqrsvwxyz` — no e, o, u, t).
- **Source paths are matched from the right, never from the left.** The compiler records a sandbox
  path — `/build/glibc-2.42/elf/x.c`, or after a `..` in `DW_AT_name` just `/sysdeps/x.c` — while
  the tree holds `glibc-2.42/sysdeps/x.c`. So the number of components to drop from the request and
  the number to add on the tree side are *both* unknown and differ per package: a tarball unpacks
  into a directory of its own, a directory-typed `.source` does not, and the overlay adds its own
  level. Trying suffixes of the request against a root only works when those two happen to cancel,
  which is why `/source/build/../sysdeps/unix/sysv/linux/getsysstats.c` 404'd in production while
  `/source/build/glibc-2.42/elf/dl-find_object.c` worked.

  `sourceIndex` instead walks the mounted tree once, keys every file by basename, and picks the
  candidate sharing the most trailing components with the request — the same idea as the deleted
  `nix_source_files.rev_file_path`, against the tree instead of a table. glibc has three
  `getsysstats.c`; this picks the right one.
- **Startup must clear mount points left by the previous process.** Mounts do not survive a
  restart — a new mount namespace — but the empty *directory* each one left behind does, because
  it lives in the persisted cache volume. The whole lazy path keys on `ENOENT`, so an empty
  leftover makes `os.Open` **succeed**: nothing is fetched, nothing is re-mounted, the index walks
  an empty tree, and every source request 404s. Only after a restart, and only until the cache is
  wiped. `reconcileMountRoots` removes them with `os.Remove` (never `RemoveAll`, so anything that
  is not an empty directory is left alone and logged).
  `TestRestartWithLeftoverMountPointsStillServes` pins it — and note that it has to unmount the
  first store by hand, because `t.Cleanup` runs at the end of the test, so without that the second
  store shares the first one's live mounts and the test passes with the fix reverted.
- **The entry image is keyed by the ARCHIVE, not the build ID.** `/debuginfo/<buildid>` returns an
  archive that many build IDs share — one glibc NAR covers 389 of them — so keying by build ID
  downloaded the same 11.8 MB once per build ID and kept that many identical images (~4.6 GB of
  transfer and ~6 GB on disk, for one package). `entry` splits the two halves: `DebuginfoMeta` is a
  159-byte JSON lookup per build ID, `FetchNarByArchive` is the expensive half and runs once per
  archive. Verified live: three build IDs, one 15.8 MB image, three symlinks.
- The build ID → archive relation is recorded as a **symlink** (`images/buildid/<id>` →
  `../archive-<key>.erofs`), which is what the relation is — hundreds of build IDs pointing at one
  archive. It reads back with no parsing, shows up in `ls -l`, and when eviction deletes an archive
  the build IDs that used it are left as dangling links, which is how you find them. Note the two
  directions are **not** symmetric: `archiveKey` parses a binary-cache path, `keyFromImageName`
  parses an image filename. Feeding the image name to `archiveKey` fails silently, so the link
  never reads back and every request re-fetches metadata it was meant to remember.
- **The member is not stored, but it is probed rather than computed.** The `.build-id` layout puts
  it at `<first two hex>/<rest>`, but there are two spellings — older NARs use no suffix, newer ones
  append `.debug` — so `memberIn` asks the mounted archive instead of guessing. It uses `Lstat`, so
  a member that is itself a symlink into another store path is accepted and resolved later by
  `Open`, rather than failing on a target that is not mounted yet.
- The archive path is **never reconstructed** from the key. Its extension is not ours to guess —
  the binary cache already moved narinfo NARs from `.nar.xz` to `.nar.zst` — so it comes from the
  lookup, which only runs when the image actually has to be rebuilt.
- **The three debuginfod response headers carry measured values, set per endpoint.** `size` is
  taken from the file about to be served, so it cannot drift from what the client receives; `file`
  is the `.executable` sibling's link target — the binary the debug info describes, e.g.
  `/nix/store/<hash>-glibc-2.42-61/lib/libc.so.6` — and empty when an archive is too old to carry
  the sibling links; `archive` is the NAR path. They are set before the body, because committing
  the status closes the header block and only trailers could follow.
- The archive path is remembered in `archive-<key>.name`, written **once per archive**, so a warm
  request can report it without a lookup. It cannot be rebuilt from the key: the binary cache
  already moved narinfo NARs from `.nar.xz` to `.nar.zst`, and `TestArchiveHeaderSurvivesRestart`
  uses a `.nar.zst` fixture precisely so a reconstruction would fail the test.
- **`cmd/nix-debuginfod` serves HTTPS with a certificate generated in memory at startup**
  (`tls.go`, `TLS_ENABLED=false` turns it off for debugging). Not for identity — it listens on
  loopback and only the proxy on the same host reaches it — but because Go's HTTP/2 server only
  runs over TLS, and HTTP/2 is what carries 103 Early Hints on a connection a client keeps open
  across many build IDs. Verified live: `http_version: 2`, `HTTP/2 103` then `HTTP/2 200`.
- **The caller must therefore skip verification, and `cmd/proxy` now does — for loopback only.**
  The upstream entry is `"nix": … URL: "https://127.0.0.1:8034"`, and the decision to skip is made
  in `DialTLSContext` from the **dial address**, not from a per-upstream flag: a flag can be set on
  the wrong entry, and that entry would silently stop authenticating a third party, whereas there
  is no way to spell fedora's host such that `isLoopbackAddr` returns true. Remote upstreams keep a
  full chain check with `ServerName` set explicitly. `TestFetchRejectsSelfSignedCertOffLoopback`
  pins it by presenting the *same* certificate from a non-loopback address.
  The certificate covers `localhost`, `127.0.0.1` and `::1`, derived from `LISTEN_ADDR` so changing
  the address cannot silently produce one for the wrong host; it is backdated a minute against
  clock skew and valid for ten years, because the only rotation is a restart.
- **A 103 Early Hints goes out as soon as the build ID resolves**, from `entry` right after
  `archiveFor`, carrying `x-status: 200` and the archive. It says "this exists and the answer will
  be 200" *without committing a status* — a 200 sent early, which the old TODO in `nix.go`
  proposed, would forfeit answering 404 or 500 when the NAR download or `mkfs.erofs` then fails.
  Placing it after `archiveFor` is what makes the promise honest: an unknown build ID fails before
  it and gets a plain 404, no hint. `x-status` is deleted after sending, so it never appears beside
  the real status.
- `writeHeaderLocked` treats 1xx as informational — sent, but not committing. Without that branch
  the first Early Hints would seal every response as "103" and the access log would never see the
  real status. `httptest.ResponseRecorder` does **not** make this distinction (it records 103 and
  then refuses the body), so the store tests wrap it — see `recorder` in `store_linux_test.go`.
- The middleware also keeps a timer-based bare 103 after `earlyHintsAfter` (2 s), for the case
  where even the lookup is slow; `sentHints` stops the two from doubling up. That one fires from
  another goroutine, which is why header writes are mutex-guarded: `Timer.Stop` does not wait for a
  callback that has already begun.
- Unverified: whether Cloudflare treats a 103 as "origin responded" for its 100 s timeout, and
  whether the elfutils client does anything with 1xx at all. Both are only measurable in
  production, and without the first one the 103 may buy nothing.
- **The mount root must exist before anything is looked up under it.** `missingFor` walks a path
  component by component and stops at the first that does not resolve; with `/nix` absent it gave
  up on `/nix` — which is not a store path and never will be — so it never reached the component it
  could have fetched. A request naming a store path directly then 404'd in 85 µs without one fetch
  attempt, while requests arriving through `.source` worked, because those start inside an
  already-mounted image. `newStore` creates it; `mountErofs` also creates ancestors, but only once
  a mount happens, and the first mount was the thing being blocked.
- **Keeping the index in ClickHouse was considered and rejected**, with numbers, so it does not get
  re-proposed. Measured on the Go stdlib tree (11476 files, 150 MB): the in-memory index is 3.2 MB
  (291 B/file), builds in 20 ms and answers in **488 ns**. A table would make every lookup a query
  round trip — roughly a thousand times slower — to save 3.2 MB. Persistence buys nothing either:
  `docker rm -f` is a SIGKILL, so the mounts are gone after a restart and every tree must be
  re-mounted anyway, next to which a 20 ms re-walk is noise. The memory is bounded by the same
  eviction that mounts and images already need; a table would only move that problem and add an
  invalidation coupling. It would earn its place only if the file *content* lived there too, so
  `/source/*` never mounted anything — a different architecture, not an optimisation of this one.
  (`filesystem()` needs ClickHouse >= 26.5, and `clickhouse local` is a 700 MB binary.)
- Two consequences worth keeping: the answer is always a path produced by walking the tree, never
  the client's string joined onto a root, so a request **structurally cannot** name a file outside
  it — no filter to forget. And `filepath.WalkDir` lstats its root, so given a symlink (which both
  `.sourceoverlay` and `.source` are) it walks nothing; `newSourceIndex` calls `EvalSymlinks`
  first. The index is cached per mounted tree — an erofs image is read-only, so it cannot go
  stale — and **is not yet evicted along with the mount**.
- The entry image needs no canonical mount point: `debuginfo/<buildid>` returns a bare
  `nar/<filehash>.nar.xz` with no store-path identity, and nothing ever links *into* it.
- **Never mount through `mount(8)`.** It sets up a loop device even when the source is named as a
  plain file with no `-o loop`, and the number of loop devices is capped by the host's `max_loop`
  (the OrbStack dev kernel caps it at **4**, which is why this is not theoretical). Production runs
  Linux 7.2 with `CONFIG_EROFS_FS_BACKED_BY_FILE=y`, so `mountErofs` (`mount_linux.go`) goes
  through `fsopen`/`fsconfig`/`fsmount`/`move_mount` instead and consumes no loop device at all.
  It falls back to `mount(8)` when the kernel answers **`ENOTBLK`** ("block device required") —
  not `EINVAL`, which is what the first version checked for and why the fallback silently never
  fired. The fallback exists so the tests run on a dev machine; `mountErofs` returns which path it
  took, and the tests log it, so a run on the real kernel visibly says `file-backed`.
- File-backed removes the loop device, **not** the privilege: mounting still needs `CAP_SYS_ADMIN`.
  It does mean the container can take `--cap-add SYS_ADMIN` instead of `--privileged` plus
  `/dev/loop-control`.
- Unmount uses `MNT_DETACH`, so evicting a mount that is still being read does not fail with
  `EBUSY` — the kernel keeps it alive until the last open fd closes. `TestUnmountWhileFileIsOpen`
  pins that, because eviction otherwise fails exactly when the cache is busiest.
- **`cache.nixos.org` serves zstd, not xz.** Every narinfo fetched from it says
  `Compression: zstd` and a `.nar.zst` URL, while the `/debuginfo/<buildid>` JSON still hands back
  a `.nar.xz` archive path. `fetchNar` therefore sniffs magic bytes (NAR / zstd / xz) instead of
  assuming xz for anything that is not a raw NAR — the old assumption made `debuginfo` work and
  `executable` and `source` fail with "xz: invalid header magic bytes".
- **erofs build flags are constrained from two sides at once.** `TarballToErofs` now passes
  `-zlz4hc` and no longer passes `-Enoinline_data`; measured on the Go stdlib source tree
  (150 MB, 11476 files, median 1766 B) that is **151.8 MB → 58.8 MB, 2.58x**. lz4 is not a
  preference: our `mkfs.erofs` (alpine erofs-utils 1.9) offers lz4/lz4hc/deflate, while the deploy
  host's kernel has `ZIP=y` and `ZIP_LZMA=y` but **not** `ZIP_DEFLATE` or `ZIP_ZSTD`. So
  `-zdeflate` builds cleanly here and cannot be mounted there — a failure that only appears in
  production — and `-zlzma` would mount there but mkfs cannot produce it. Check
  `zgrep EROFS /proc/config.gz` on the host before touching this.
- `CONFIG_EROFS_FS=m` on the host: erofs is a **module**. A container generally cannot autoload
  one (that needs `CAP_SYS_MODULE` in the initial namespace), so it has to be loaded on the host
  before the service starts — `/etc/modules-load.d/erofs.conf`. Testing the mount by hand as root
  on the host loads it as a side effect and hides this.

The `nix/` package is the intended replacement pipeline and *does* compile. Instead of caching raw
NARs it streams NAR → tar → **erofs image** through an `io.Pipe` with an `errgroup`
(`FetchNarByBuildID` / `FetchNarByStorePath` in `nix/nix.go`), so individual files can later be read
random-access via `github.com/erofs/go-erofs`. Requires **`mkfs.erofs` in `$PATH`** — the
constructor `log.Fatal`s without it. Roughly 1GB in ~60s.

`nix-debuginfod`'s `db.go` now does one thing: it writes `nix_access_log`, one row per request. The
`nix_source_files` index and its lookup are gone (see above). `AccessLog` used to insert into
`access_log` — the proxy's table, which would have folded this service's traffic into the proxy's
`/stats` — and now writes `nix_access_log`, which is what `Init` creates.

`nix_access_log` deliberately is **not** `access_log`. It shares the first ten columns exactly
(`duration_100kb_ms` was added at the same position, 9) and then drops two the proxy has:

- `resolved_host` — the proxy fans out to twelve upstreams and records which one answered; this
  service has one, hardcoded in `nix/nix.go`, so the column only ever held `''`.
- `cache_status` — not dropped on purpose, just never wired up. Unlike `resolved_host` it has real
  meaning here (image on disk / had to build / coalesced onto another build / waited for a slot),
  and filling it needs the store to report what happened back to the middleware.

So `SELECT *` does not line up across the two tables and a union has to name columns. That is the
accepted trade: a column that is always empty reads as if it means something.

`nix_access_log` **has never been created in production**, so everything in `Init` is still the
initial schema and carries no `ALTER`. That stops being true on the first deploy — after it, any
new column needs its own `ALTER TABLE … ADD COLUMN IF NOT EXISTS` beside the CREATE.

`throughputSampleBytes` must stay identical in `cmd/proxy` and `cmd/nix-debuginfod`
(`TestThroughputSampleMatchesProxy` pins it): both write the same column name into tables meant to
be queried the same way, and a different threshold would quietly make the numbers mean different
things.

`response_headers` in that table is always empty: the middleware reads `x-debuginfod-*` headers and
no handler here sets any. Either start emitting them (the protocol has them) or drop the column.

### Debian source backend (`cmd/deb-debuginfod` + `cmd/deb-unpack`, WIP)

**No index of our own is needed, and that is the whole reason this is cheap.** `debuginfod.debian.net`
answers `/buildid/<id>/debuginfo` with an `X-DEBUGINFOD-ARCHIVE` header naming the exact `.deb`:

```
/srv/mirror/debian-debug/dbg-main/g/glibc/libc6-dbg_2.41-12+deb13u3_amd64.deb
```

Mapping build IDs to packages is otherwise a mirror-sized job — it is what `cmd/indexer` was
originally for — and Debian is already doing it. `parseArchive` turns that header into a source
package. Two details come from real headers and are easy to get wrong: the component is `dbg-main`
but the **sources** live in plain `main`, so the prefix is stripped; and the source package is the
pool **directory** (`glibc`), never the binary name in the filename (`libc6-dbg`, which is also
pre-`dbgsym` naming). A binNMU `+bN` suffix has no `.dsc` of its own and is stripped.

**The sources must be patched, and this is not a detail.** Debian's 3.0 (quilt) format keeps the
pristine tarball and a patch series side by side. For glibc 2.41-12+deb13u3 the series is 94 patches
changing **666 source files**, `elf/rtld.c` among them by 97 lines — and `rtld.c` is a file clients
really request. Serving pristine there shows a debugger the wrong lines, silently. `dpkg-source -x`
does the whole job in 1.2 s; reimplementing quilt semantics in Go was considered and rejected, since
a partial version fails on exactly the packages nobody tests.

**That is why there are two commands.** `dpkg-source` needs `dpkg-dev` (20 packages, ~94 MB), so
`cmd/deb-unpack` runs in `debian:13-slim` and owns it, while `cmd/deb-debuginfod` stays on a
distroless image and talks to it over HTTP across a shared volume. Shelling out to `docker run`
instead was rejected: it needs the docker socket, which is root on the host.

- **All policy lives in the serving side.** The unpacker is stateless and does one thing,
  `(package, version) → tree on the volume`. Caching, the index, coalescing and eviction belong to
  `cmd/deb-debuginfod`, because two services with opinions about what is cached is how they start
  disagreeing.
- **Path matching is `srcindex`, shared with `cmd/nix-debuginfod`.** Necessary, not decorative:
  clients join `DW_AT_comp_dir` with `DW_AT_name`, which for glibc produces `./elf/./elf/do-rel.h` —
  a doubled component that normalisation alone resolves to a path that does not exist. Matching from
  the right finds `elf/do-rel.h`. Verified against all four shapes seen in production logs.
- **Build ID → package is cached** (`expirable.LRU`, 10k/24 h) and coalesced. Without it every
  source request costs a round trip to Debian: measured **140 ms → 0.3 ms** warm. Failures are
  deliberately **not** cached — a failure is usually "Debian was slow", and a 24 h TTL would turn a
  blip into a day-long outage for that build ID.
- `--no-check` skips the `.dsc` signature check. Deliberate: verifying needs the Debian keyring in
  that image, and the bytes came over TLS from the archive that signed them. Revisit before pointing
  this at an untrusted mirror.

Measured on glibc: 20 MB of archives → **251 MB unpacked, 20,961 files**; cold request 2.7 s, warm
0.3 ms.

**Eviction is LRU by request, and the split between the services is the interesting part.** The
serving container mounts the volume `:ro`, so it cannot delete — which is deliberate: the unpacker
is the single writer, and a mistake in the serving code cannot corrupt the cache. So the server
decides (it is the only one that sees traffic) and calls `DELETE /tree/{component}/{name}` on the
unpacker to act. Two orderings are load-bearing and have tests:

- The **index is dropped before the delete is requested**. The other way round leaves a window where
  the index still offers paths whose files are gone, and every request in it 502s instead of paying
  for a re-unpack.
- The unpacker **renames before RemoveAll**, so a tree stops being visible instantly rather than
  spending a moment existing with its done-marker and half its files. A reader holding it open is
  fine: unlink keeps the data alive until the last descriptor closes.
- The done-marker is written into the **staging** directory before the atomic rename, so a
  package-named directory always implies a complete tree — there is no window in which a final name
  exists without one. A directory that nonetheless lacks the marker came from somewhere else (a
  restored backup, an older build) and `sweepStale` **reports it and leaves it alone**: it is
  invisible to `/trees` and so never evicted, but deleting an operator's directory is the worse of
  the two mistakes.

Ordering is by last *request*, not mtime — mtime cannot tell a package fetched once from one serving
a debugger all week — with mtime as the fallback for trees this process has never served, e.g. after
a restart. A zero or negative budget **disables** eviction rather than emptying the volume, so a typo
in the config does not delete everything on the first tick. Verified end to end: 297 MB on the
volume, 200 MB budget, glibc evicted, 3.1 MB left, and the next request for it rebuilt in 2.1 s.

`CACHE_MAX_BYTES` (20 GiB) and `EVICT_INTERVAL` (10 m) configure it. Pruning to `.c/.h/.S` would cut 251 MB to ~110 MB but was
**rejected**: DWARF names compilation units, while headers come from the line table, so the servable
set is open-ended and pruning by extension would reintroduce exactly the silent-wrong-answer class
the patching fixes.

`sync.sh` sends `srcindex/` and both Dockerfiles, and `run.sh --deb` builds and starts the pair.
Note `sync.sh` lists top-level packages **explicitly**, not by glob: forgetting one is a remote-only
failure, where the image builds locally and its `COPY` fails on the host.

**`cmd/proxy` routes to it through a delegate, not an upstream.** `Server.SourceVia` names a server
that serves `/source/*` for build IDs this one resolves but cannot supply sources for; `debian`
points at `debian-src`, and `applySourceRules` (the single place where "resolved but no sources"
becomes a 501, shared by the fast and slow paths) hands the request over instead.

Delegates live in their own map and are **deliberately not in `servers`**: `tryAllServers` probes
every entry there with `/buildid/<id>/debuginfo`, and a source-only backend answers 501 to that — so
it would be probed on every cold build ID, fail every time, and appear in `resolve_logs` and on
`/stats` as a backend with a 0% success rate. `TestDelegateWiringIsConsistent` pins both directions.

Two consequences that were not obvious:

- The delegate appears in `access_log.resolved_host` and never in `resolve_logs`, so the `/stats`
  offline badge would have marked it offline on every page load. `isOffline` now requires probe
  history: "stopped being probed" says nothing about something that was never in the rotation.
- It must be on **loopback**, and not just as a deployment preference: `fetchClientFor` gives
  loopback the client with no response-header timeout, and a source backend writes headers only
  after unpacking a package — seconds, or minutes for a large one. A remote delegate would be cut
  off at the 5 s limit on every cold package. The same test pins it.

### ClickHouse tables

Created by `db.Init()` in each service.

- `buildid_state` — `ReplacingMergeTree(updated_at) ORDER BY buildid`. Writes go through
  `AsyncInsert`; reads use `ORDER BY updated_at DESC LIMIT 1` since dedup is not guaranteed to have
  run yet.
- `resolve_logs` — one row per upstream probe per resolution (so ~10 rows per cold build ID).
- `access_log` — per-request, `PARTITION BY toYYYYMM(timestamp)`, includes a
  `Tuple(size, file, archive, imasignature)` of debuginfod response headers, plus `country`
  (`CF-IPCountry`) and `tags` (`Array(LowCardinality(String))`, currently only `github_actions`).
  Both arrived with an `ALTER … ADD COLUMN IF NOT EXISTS` beside the `CREATE`
  (`TestDBAccessLogMigrationAddsTagsAndCountry`) — without it every deployed instance would keep
  the old table and every `INSERT` would fail, which is worse than the `cache_stats` case since
  this is the table every request writes.
- `github_download_stats` — created and written by `cmd/releases`, not by the proxy
- `releases_access_log` — one row per release redirect: `version`, `file` (both straight from the
  route match, not parsed back out of the URI), `request_uri`, `status`, `user_agent`, `country`
  from `CF-IPCountry`. Deliberately **not** a copy of `access_log`: everything that table records
  about proxying debuginfo is empty for a 302. `cmd/releases/MIGRATION.sql` moves the historical
  rows out of `access_log` (`endpoint_name = 'releases'`) and deletes them there. **Already run** —
  `access_log` now holds only `debuginfo`, `executable` and `source`, which is why the clients panel
  filtering with `endpoint_name != 'releases'` had quietly become "every endpoint"; it lists
  `statsEndpoints` positively now, pinned by `TestStatsPanelsAgreeOnEndpoints`
- `cache_stats` — one row per measurement of `CACHE_PATH`: blob count, allocated bytes
  (`st_blocks`) alongside apparent bytes, bytes tied up in abandoned `.tmp-*`, plus partition
  size and free space from `statfs`
- `nix_access_log` — created by `cmd/nix-debuginfod` only.
- `pbs_buildids`, `pbs_assets` — created and written by `cmd/indexer` only; nothing reads them yet.
  `pbs_buildids` is build ID → (release asset, member path); `pbs_assets` is the resume log, keyed
  on the asset's compressed size, since a published release asset is immutable. Neither has ever
  existed in production, so both are still initial schema and carry no `ALTER` — that stops being
  true on the first run against the real instance.

Access-log and stats inserts use `PrepareBatch` with `clickhouse.WithStdAsync(false)`; state updates
use `AsyncInsert`. DB calls take a 5s timeout (60s for source indexing).

## API

Debuginfod protocol (proxy and both nix services):
`GET /buildid/:buildid/{executable,debuginfo,source/*path}`

Proxy only: `GET /status`, `GET /stats` (background-rebuilt traffic charts). Anything unrouted,
`/` included, 302s to the project page.

`cmd/releases`: `GET /releases/:version/:file` (302 to pwndbg GitHub releases) and `GET /stats`,
both `Host`-restricted to `releases.pwndbg.re`. Its `/stats` collects **per view** rather than
slicing one long window like the proxy does: the headline is a distinct-IP count, and distinct
counts cannot be re-aggregated from daily ones. Asset names are bucketed in Go (`classifyAsset`),
not in SQL, so the split is unit-tested and can change without rewriting stored rows; the
User-Agent buckets stay in SQL and mirror what the Grafana dashboard already used, because
Homebrew's architecture is only knowable from its User-Agent.

The proxy forwards **no** client headers upstream. It sends a fixed `user-agent`
(`upstreamUserAgent`, set centrally in `finder.Fetch` so no call site can forget it — the
resolution probes used to send Go's default) and **overrides** `accept-encoding` with a fixed
`gzip` (`upstreamAcceptEncoding`), so the cache holds exactly one representation per artifact
regardless of what the client asked for. That representation is served **verbatim to every
client** — there is deliberately no content negotiation and no on-the-fly decompression,
because all traffic arrives via Cloudflare, which settles the encoding with the end client.
One representation also means no `Vary` header is required. It copies back the
`x-debuginfod-*`, `content-type`, `content-encoding` and `content-length` headers plus its own
`x-server` (the host that actually produced the bytes, taken from the cache entry on a HIT)
and `x-cache` (`HIT`/`MISS`/`COALESCED`/`BYPASS`/`OVERLOADED`). Build IDs are checked for
length (32–64) but **not** for character set. `WriteTimeout` is 60 minutes because debuginfo
downloads are large, while `cacheFetchTimeout` in `filecache.go` is 30 — a detached download
is cut at half the client's connection budget, deliberately. Nothing enforces the relationship
between the two literals.

Cache keys are built from `endpointName + buildid + path` (`cacheKey`), never from
`r.RequestURI` — httprouter matches on the path, so a query string reaches the handler without
changing the response, and keying on it let one client mint unlimited blobs and upstream fetches.

There is no `/metrics/github` route — GitHub download counts go to `github_download_stats` via a
background worker and are read from Grafana. The README used to document a route that
`InitRouter` never registered; that claim is gone.

## Grafana

Provisioned from `grafana/` — a ClickHouse datasource plus three dashboards: `/d/github-downloads`,
`/d/buildid-resolution`, `/d/access-logs`.
