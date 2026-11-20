package main

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func NewDB(ctx context.Context, dsn string) (*dbSrv, error) {
	options, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, err
	}
	return &dbSrv{conn: conn}, nil
}

type dbSrv struct {
	conn driver.Conn
}

func (s *dbSrv) Close() error {
	if conn := s.conn; conn != nil {
		return conn.Close()
	}
	return nil
}

// indexed_buildids maps a build id to an archive holding it, and to the path
// inside that archive. cmd/index-debuginfod reads it; this command writes it.
//
// One table for every source, so the serving side is one query and knows
// nothing about GitHub releases or Ubuntu pools: url + format + member is
// enough to fetch and extract, kind says which endpoints may answer, and
// archive is the x-debuginfod-archive to send.
//
// A build id may appear under several urls - the same .deb from the pool and
// from Launchpad, say - and the server tries them in turn.
//
// ReplacingMergeTree on indexed_at so re-indexing an archive supersedes its
// rows instead of doubling them.
const createBuildIDs = `
CREATE TABLE IF NOT EXISTS indexed_buildids (
    buildid     String,                  -- lowercase hex, as the ELF note spells it
    source      LowCardinality(String),  -- pbs, ubuntu
    kind        LowCardinality(String),  -- executable (unstripped) or debuginfo (separate .debug)
    package     String,                  -- cpython, libc6-dbg
    version     String,                  -- 3.14.7+20260825, 2.43-2ubuntu2.7
    arch        LowCardinality(String),  -- x86_64-unknown-linux-gnu, amd64
    url         String,                  -- where to fetch the archive
    format      LowCardinality(String),  -- deb, tar.gz, tar.zst, tar.xz
    archive     String,                  -- the x-debuginfod-archive value
    member      String,                  -- path inside the archive's file tree
    member_size UInt64,                  -- uncompressed size of that member
    indexed_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY (buildid, source, url, member);
`

// indexed_archives is the resume log: one row per archive the indexer has
// scanned.
//
// Scanning means downloading - ~126 MB for a python asset - so skipping what
// has not changed is the difference between an incremental run and re-reading
// everything. Both sources publish immutable files under versioned names, so
// the url settles it; the size is compared as well where the listing gives one.
const createArchives = `
CREATE TABLE IF NOT EXISTS indexed_archives (
    url         String,
    source      LowCardinality(String),
    name        String,
    size        UInt64,                  -- compressed size when known, else 0
    buildids    UInt32,                  -- how many this archive yielded
    error       String,                  -- '' on success
    indexed_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY url;
`

// indexed_releases closes python-build-standalone releases: one row per release
// whose every selected asset is in indexed_archives. A closed release is not
// listed again - its asset listing is ten API pages - so a periodic run costs
// one request for the tag names and nothing for the past. Keyed on the variant
// set as well, so adding a variant reopens everything.
const createReleases = `
CREATE TABLE IF NOT EXISTS indexed_releases (
    source      LowCardinality(String),
    release     String,
    variants    String,                  -- sorted, comma-separated
    indexed_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY (source, release, variants);
`

// Init creates the tables this command owns.
//
// Any column added here later needs its own ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS beside the CREATE: on a table that already exists CREATE TABLE IF NOT
// EXISTS is a no-op and silently does not add it, which is how apparent_bytes
// broke cache_stats in production.
func (s *dbSrv) Init(ctx context.Context) error {
	for _, stmt := range []string{createBuildIDs, createArchives, createReleases} {
		if err := s.conn.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// archiveState is what the resume check compares against.
type archiveState struct {
	Size uint64
}

// LoadState reads the resume log into memory: one round trip beats a query per
// archive. Failed rows are left out, so a download that broke halfway is
// retried on the next run rather than remembered as done - except a 404, which
// says the file is gone for good and would otherwise be asked for forever.
func (s *dbSrv) LoadState(ctx context.Context) (map[string]archiveState, error) {
	rows, err := s.conn.Query(ctx, `
SELECT url,
       argMax(size, indexed_at)  AS size,
       argMax(error, indexed_at) AS error
FROM indexed_archives
GROUP BY url
HAVING error = '' OR error = ?`, errGone.Error())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]archiveState{}
	for rows.Next() {
		var (
			u    string
			size uint64
			errS string
		)
		if err := rows.Scan(&u, &size, &errS); err != nil {
			return nil, err
		}
		out[u] = archiveState{Size: size}
	}
	return out, rows.Err()
}

// artifactRow is one indexed_buildids row.
type artifactRow struct {
	Artifact
	Archive Archive
}

// archiveRow is one indexed_archives row.
type archiveRow struct {
	Archive  Archive
	BuildIDs uint32
	Err      string
}

// InsertBatch writes one batch. Build ids go in before the archives that
// produced them are marked done, so a crash between the two makes the next run
// redo an archive rather than skip one that was never fully written.
func (s *dbSrv) InsertBatch(ctx context.Context, arts []artifactRow, archives []archiveRow) error {
	if len(arts) > 0 {
		batch, err := s.conn.PrepareBatch(ctx, `
INSERT INTO indexed_buildids (buildid, source, kind, package, version, arch, url, format, archive, member, member_size)`,
			driver.WithReleaseConnection())
		if err != nil {
			return err
		}
		for _, a := range arts {
			ar := a.Archive
			err := batch.Append(a.BuildID, ar.Source, ar.Kind, ar.Package, ar.Version, ar.Arch,
				ar.URL, ar.Format, ar.Header, a.Member, uint64(a.Size))
			if err != nil {
				batch.Abort()
				return err
			}
		}
		if err := batch.Send(); err != nil {
			return err
		}
	}

	if len(archives) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, `
INSERT INTO indexed_archives (url, source, name, size, buildids, error)`, driver.WithReleaseConnection())
	if err != nil {
		return err
	}
	for _, a := range archives {
		err := batch.Append(a.Archive.URL, a.Archive.Source, a.Archive.Name, uint64(a.Archive.Size), a.BuildIDs, a.Err)
		if err != nil {
			batch.Abort()
			return err
		}
	}
	return batch.Send()
}

func (s *dbSrv) DoneReleases(ctx context.Context, variants string) (map[string]bool, error) {
	rows, err := s.conn.Query(ctx, `
SELECT DISTINCT release FROM indexed_releases WHERE source = ? AND variants = ?`, sourcePBS, variants)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out[tag] = true
	}
	return out, rows.Err()
}

func (s *dbSrv) MarkReleasesDone(ctx context.Context, variants string, tags []string) error {
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO indexed_releases (source, release, variants)`,
		driver.WithReleaseConnection())
	if err != nil {
		return err
	}
	for _, t := range tags {
		if err := batch.Append(sourcePBS, t, variants); err != nil {
			batch.Abort()
			return err
		}
	}
	return batch.Send()
}
