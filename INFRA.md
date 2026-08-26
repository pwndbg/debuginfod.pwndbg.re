# Infrastructure

Operational reference for `debuginfod.pwndbg.re`.

Everything here is verifiable from this repository **except** the Cloudflare Tunnel, the Cache
Rule, the DNS records and the uptime monitor. Those live on the host and in the Cloudflare
dashboard; lines marked **(external)** are the only record of them.

## Where things run

Host `host1.cypis.ovh`, deploy root `/persist/debuginfod.pwndbg.re/`.
All containers use `--network host`, so `EXPOSE` in the Dockerfiles is decorative.

| Service | Address          | Container | Source                                              |
|---|------------------|---|-----------------------------------------------------|
| proxy | `127.0.0.1:8031` | `pwndbg-debuginfod-proxy` | `cmd/proxy`                                         |
| nix backend | `127.0.0.1:8034` | `pwndbg-debuginfod-nix` | `cmd/nix-debuginfod`, one upstream among the others |
| releases | `127.0.0.1:8033` | `pwndbg-debuginfod-releases` | `cmd/releases`, serves `releases.pwndbg.re` only    |
| ClickHouse | `127.0.0.1:9000` | `clickhouse` | `clickhouse-server:25.3-alpine`                     |
| cloudflared | —                | — | system daemon **(external)**                        |

Volumes: `ch_data`, `ch_logs`, `pwndbg_debuginfod_cache`. All survive `docker rm -f`.

`CACHE_PATH=/var/lib/pwndbg-debuginfod-cache`, on **btrfs**. `run.sh` does not set
`CACHE_MAX_BYTES`, so the 50 GiB default applies regardless of volume size.

## Deploy

```bash
./sync.sh          # rsync --delete to root@host1.cypis.ovh:/persist/debuginfod.pwndbg.re/
./run.sh           # on the host: rebuild and recreate the proxy only
./run.sh --all     # also ClickHouse, releases and the nix backend
```

`sync.sh` deletes on the remote side. It has to: `go build` compiles whatever is in the package
directory, so a file deleted locally but left on the host breaks the build there while it still
builds here. Deletion is scoped to the transferred trees, not to the whole deploy root.

## Cloudflare

**(external)** `cloudflared` runs on the host and opens an outbound tunnel; requests arrive through
it at `127.0.0.1:8031`. No inbound ports are open. Client IPs come from `CF-Connecting-IP`, trusted
because the tunnel is the only way in — any local process can forge it.

Edge cache, set as a Cache Rule in the dashboard **(external)**:

| Status | Edge TTL |
|---|---|
| `200` | 1 year |
| `404`, `501` | 2 hours (plan minimum) |
| everything else | not cached |

The rule is scoped to a path prefix, not site-wide: `/status` answers `DYNAMIC`.
Purging is dashboard/API only — deleting a blob locally does not touch the edge.

Check what the edge did with a URL:

```bash
curl -sS -o /dev/null -D- https://debuginfod.pwndbg.re/buildid/<id>/debuginfo \
  | grep -iE '^(HTTP/|cf-cache-status|age|cf-ray):'
```

`MISS`→`HIT` = cached · `DYNAMIC` = not cached · `BYPASS` = rule says don't cache.
The `cf-ray` suffix is the colo; two `MISS`es may just be two different colos.

## Caches

Four of them. The inner two cache *resolution*, the outer two cache *bytes*.

| Layer | Lifetime | Key |
|---|---|---|
| Cloudflare edge | see table above | request URL |
| disk (`filecache.go`) | until LRU eviction | sha256 of `endpoint + buildid + path` |
| `buildid_state` | until overwritten | build ID |
| in-process LRU (`finder.go`) | 10k entries / 24 h | build ID |

- Resolution runs **before** the disk cache, so a HIT still costs a `FindByBuildID`.
- Negative results back off on `state.Counter`: 30 min → 1 h → 2 h → 24 h, then refused past 30.

## Monitoring

- `/stats` on `debuginfod.pwndbg.re` — 7/30/180/360-day charts, rebuilt hourly in the background,
  served from memory.
- `/stats` on `releases.pwndbg.re` — who downloads which release artifact, from
  `releases_access_log`, alongside GitHub's own counters. The two are separate sections because
  they measure different things: ours counts redirects issued (a client still has to follow one,
  and a retry asks twice), GitHub's counts real downloads from everywhere, including people who
  never touched this host.
- `github_download_stats` — filled hourly by `cmd/releases`, not by the proxy. Cumulative, and
  polled only for whichever release is `latest`, so an older tag freezes when its successor ships.
- `GET /status` — polled by an uptime monitor, status page <https://pwndbg.upbot.app>
  **(external)**. Returns 200 unconditionally.

## Assumptions

Break one of these and something fails quietly.

- **Everything arrives through Cloudflare.** The origin is loopback-only. Most of the rest depends
  on this. `CF-Connecting-IP` and `CF-IPCountry` are both believed only from a trusted peer, and
  loopback counts as trusted — so any process on this host can write its own address and country
  into `access_log`, and with `--network host` that means the whole machine.
- **Cloudflare accepts `Content-Encoding: gzip` regardless of the client's `Accept-Encoding`.** We
  ignore that header entirely — `writeBody` never reads it. RFC 9110 forbids this; the CDN is what
  makes it safe.
- **Cloudflare does not compress `application/octet-stream`.** That is why we compress on ingest at
  all.
- **One representation per artifact, so no `Vary`.** Reintroducing content negotiation makes
  `Vary: Accept-Encoding` mandatory in the same commit.
- **A build ID is unique across upstreams** (sha1 over the linked output). At most one upstream ever
  has it, which is what makes pinning `state.LastSuccess` correct. When the pinned host drops an
  artifact, nobody else has it either.
- **Build IDs are content-derived, not random.** Every distro here uses `--build-id=sha1`; md5 would
  do as well. `uuid` is equally legal and mints a fresh random ID on every link — 32 hex characters,
  so our 32–64 validation would let it through. Nothing would then tie an ID to its bytes: a rebuild
  of the same source stops matching what users have, identical content stops deduplicating, and an
  artifact can only be checked against its key by reading the ELF note, never by hashing.
- **ClickHouse is on the request path.** An outage 500s every request, including artifacts complete
  on disk. Only the in-process LRU keeps answering.
- **The cache filesystem is btrfs, and the cache is disposable.** `filecache.go` skips `fsync` and
  stores no checksums, so it leans on the filesystem: btrfs data checksums turn a bad block into an
  EIO rather than silently wrong bytes, and CoW means a crash loses the last transaction instead of
  leaving a right-length file full of garbage. `openCached` only compares length against
  `meta.Size`, so on ext4/xfs the same design could serve corrupt debuginfo. Acceptable while a
  lost entry costs one re-download; not if the cache becomes the only copy of dropped artifacts.
- **`docker rm -f` is a SIGKILL.** Nothing deferred runs on redeploy; eviction reclaims abandoned
  `.tmp-*` by age.

## Gotchas

Known-wrong on purpose, or known-wrong and unfixed.

- **A slow upstream reads as a missing artifact.** `tryAllServers` has 5 s; losing that race gives a
  404, bumps `Counter`, and the 404 is edge-cached for 2 h.
- **An upstream returning `200` for build IDs it lacks poisons everything.** `Fetch` checks the
  status code and nothing else — no ELF parse, no build-ID note comparison. Such a server is also
  the *fastest*, so it wins nearly every race, gets pinned via `LastSuccess`, and its bytes reach
  disk and the edge. Realistic trigger: a lapsed upstream domain serving a parking page. The check
  that would catch it is described at `filecache.go:405`.
- **`Down: true` stops us serving from a host, not asking it.** Read only at `finder.go:297` and
  `:357`, both on the fast path; `tryAllServers` ignores it.
- **Disabling an upstream poisons `buildid_state` permanently.** Build IDs only it serves climb past
  `Counter > 30` during the outage; that check has no time component and nothing resets the counter,
  so re-enabling the host changes nothing.
- **A broken pinned host gives a permanent 500 with no feedback.** Fetch errors never reach
  `UpdateState`. Nothing demotes the host; the only remedy is `Down: true` and a redeploy.
- **`/source/*` has no disk cache; the edge is its only one** (`cacheableEndpoints`). It should
  have one — the blocker is that a single build ID can mean thousands of small files, so it
  needs a different layout than one blob per key. `cacheKey` already carries the path tail.
- **No `Range` support.** A transfer that breaks at 90% restarts from zero.
- **Eviction measures apparent size, `cache_stats` measures allocated.** On btrfs the budget bites
  earlier than the disk requires.
- **`/stats` counts origin load, not client demand.** Edge hits never reach `access_log`.
- **GitHub Actions traffic is excluded from `/stats`**, from the `tags` written when each row was
  logged. It is 89% of all traffic, so the page describes a very different service with the filter
  than without it. Two consequences: the upstream-probe panel is *not* filtered, because
  `resolve_logs` records no client address; and a request logged before the range list loads is
  tagged `unclassified` and **counted**, since nothing can classify it afterwards.
  `scripts/backfill_tags.py` repairs those. Turning `GH_RANGES_ENABLED` off makes every row
  `unclassified`, i.e. no filtering at all.
- **Release-redirect history lives in a second table.** It used to be `access_log` rows under
  `endpoint_name = 'releases'`; `cmd/releases/MIGRATION.sql` (already run) moved them into `releases_access_log`
  and deletes them from `access_log`. Anything still querying the old location — the
  `/d/github-downloads` Grafana panel did — has to be repointed in the same change.
- **`country` is empty for every migrated row**, and for all rows if Cloudflare's IP geolocation
  setting is off, since it is read straight from `CF-IPCountry`. The panel then reads `unknown`
  rather than disappearing.

### Repairing poisoned `buildid_state`

`ReplacingMergeTree(updated_at)`, so a superseding row wins — no mutation needed.
**Not tested against production:**

```sql
INSERT INTO buildid_state
(buildid, last_host, last_error, counter, last_success, updated_at, response_headers)
SELECT buildid, '', '', 0, false, now(), tuple(0, '', '', '')
FROM (
       SELECT buildid,
              argMax(counter, updated_at)      AS counter,
              argMax(last_success, updated_at) AS ok
       FROM buildid_state GROUP BY buildid
     )
WHERE counter > 30 AND ok = false
```

The finder caches state in memory for 24 h, so this takes effect after that or after a restart.
If bad bytes were cached too, delete the blobs and purge the edge as well.

## Unknowns

- Where the `cloudflared` config lives and what its ingress rules are. It now needs a second
  ingress rule: `releases.pwndbg.re` → `127.0.0.1:8033`.
