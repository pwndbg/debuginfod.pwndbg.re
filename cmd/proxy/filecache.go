package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// In place of compress/gzip. The API is signature-compatible and so is the
	// output format - the tests read these blobs with the standard compress/gzip on
	// purpose.
	//
	// Only the compressor is used from here: with content negotiation removed the
	// proxy never decompresses (grepping for gzip.NewReader outside tests finds
	// nothing). Measured on a 15.4 MiB binary with symbols: 88 ms instead of 256 ms
	// (2.9x) at the cost of a 1.2% larger file. The cost lands in the wrong place -
	// compression sits inside the read loop under the maxConcurrentDownloads
	// semaphore.
	//
	// This replaces, rather than complements, the idea of dropping to
	// gzip.BestSpeed: that gave 112 MB/s at the cost of 10% size, this gives
	// 184 MB/s at the cost of 1.2%.
	//
	// The module was already pinned in go.mod (indirectly, via ClickHouse), but only
	// the zstd subtree was linked - gzip and flate now join the binary: +199 KiB on
	// linux/amd64 (one-off) and roughly +250 KiB of live heap per concurrent
	// gzip.Writer, i.e. +8 MiB at maxConcurrentDownloads.
	"github.com/klauspost/compress/gzip"
)

// proxyHeaders are the response headers copied from the upstream to the client.
// The same list goes into the cache so a HIT looks identical to a MISS.
var proxyHeaders = []string{
	"x-debuginfod-size",
	"x-debuginfod-archive",
	"x-debuginfod-file",
	"x-debuginfod-imasignature",
	"content-type",
	"content-encoding",
	"content-length",
}

const (
	// The download is detached from the leader's request, so it needs a deadline of
	// its own.
	//
	// SHORTER than the server's WriteTimeout (60 min, main.go): a download dies
	// after 30 minutes even though the client's connection would live twice as long.
	// Deliberate - a transfer dragging on past half an hour still holds a semaphore
	// slot, a descriptor and a goroutine, and nothing reclaims them. Nothing enforces
	// this relationship except this sentence: both values are literals in different
	// files.
	cacheFetchTimeout = 30 * time.Minute
	cacheEvictPeriod  = 10 * time.Minute
	// After eviction we drop below the limit with room to spare, so we do not delete on every cycle.
	cacheEvictTarget = 0.9
	cacheChunkSize   = 256 << 10
	// ingestBufSize - the buffer between gzip and the file. Smaller than
	// cacheChunkSize on purpose; see the comment where it is created in download().
	ingestBufSize = 32 << 10
	// Forced on the upstream. One fixed encoding = one representation in the cache.
	// gzip, because every upstream supports it and debuginfo shrinks ~2.3x.
	upstreamAcceptEncoding = "gzip"
	// Downloads are detached from the client, so nothing bounds them on its own: a
	// client can fire N requests and disconnect, and each one holds a goroutine, a
	// 256 KiB buffer, a descriptor and a full upstream transfer for an hour. The
	// semaphore provides the ceiling.
	maxConcurrentDownloads = 32
)

// cacheableEndpoints - /source/* is deliberately absent: a single build ID can mean
// thousands of small source files, which blows up the inode count and slows eviction.
var cacheableEndpoints = map[string]bool{
	"debuginfo":  true,
	"executable": true,
	"source":     false,
}

type cacheMeta struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Size    int64             `json:"size"`
	// The host that ACTUALLY produced these bytes. The cache key is host-independent
	// and FindByBuildID resolves afresh on every request and may pick a different
	// one - without this a HIT would return x-debuginfod-archive from one host under
	// an x-server header naming another, and access_log would lie the same way.
	Host string `json:"host"`
}

// clone breaks the sharing of the Headers map. Without it a "copy" of cacheMeta
// still aliases the producer's map and every later write to it races with the
// followers reading it.
func (m cacheMeta) clone() cacheMeta {
	headers := make(map[string]string, len(m.Headers))
	for k, v := range m.Headers {
		headers[k] = v
	}
	m.Headers = headers
	return m
}

// cacheEntry describes a download IN PROGRESS. The leader appends to tmpPath and
// advances size; followers read that same file through their own descriptors and
// wait on the condvar when they catch up. Every client therefore moves at its own
// pace - a slow one blocks nobody (which io.MultiWriter cannot manage), and a late
// one reads from byte zero what is already on disk and catches up smoothly.
type cacheEntry struct {
	mu   sync.Mutex
	cond *sync.Cond
	// tmpFile is the leader's descriptor. Followers inherit their OWN copy via dup
	// instead of opening the file by path - the path disappears on rename, the inode
	// does not. nil means the download has finished and the content now lives under
	// its final name.
	tmpFile      *os.File
	headersReady bool
	meta         cacheMeta
	size         int64
	done         bool
	err          error
}

func newCacheEntry() *cacheEntry {
	e := &cacheEntry{}
	e.cond = sync.NewCond(&e.mu)
	return e
}

// ErrCacheBusy - no free download slot. This is NOT a request error: the caller
// should bypass the cache and stream straight from the upstream, as it did before
// the cache existed.
var ErrCacheBusy = stderrors.New("cache: brak wolnego slotu na pobranie")

type fetchFn func(ctx context.Context) (*http.Response, error)

// dupFile gives a follower its OWN descriptor onto the same inode. Its own, because
// it has its own lifetime (Close does not touch the leader's descriptor). The shared
// file offset that dup carries along is irrelevant - entryReader reads via ReadAt.
func dupFile(f *os.File, name string) (*os.File, error) {
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	// The name must NOT be f.Name(), i.e. the .tmp-* path: it disappears on rename,
	// so every follower's *os.PathError would point at a file nobody can inspect.
	return os.NewFile(uintptr(fd), name), nil
}

// writeFileAtomic publishes a file via tmp+rename, so a reader never sees a version
// truncated by O_TRUNC.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // a no-op after a successful rename
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// DELIBERATELY no tmp.Sync(). No follower sees EOF until the defer sets e.done,
	// and that defer waits for everything below - an fsync of 50 MiB is ~17 ms on
	// local NVMe and seconds on a network volume, charged to EVERY concurrent
	// client. The cache is reproducible: a crash costs one re-download, so a
	// durability barrier buys nothing here.
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// countingWriter counts the bytes actually written to disk. With compression on
// ingest the number of bytes read drifts away from the file size, and it is the file
// size that followers are waiting on.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type fileCache struct {
	dir      string
	maxBytes int64
	sem      chan struct{} // ceiling on concurrent downloads

	mu       sync.Mutex
	inflight map[string]*cacheEntry
}

func newFileCache(dir string, maxBytes int64) (*fileCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		// Easy to land here while trying to "disable" the cache - this disables only THE LIMIT.
		log.Warn("cache: CACHE_MAX_BYTES <= 0, eviction wylaczony, katalog bedzie rosl bez ograniczen (wylacznik to CACHE_ENABLED=false)")
	}
	return &fileCache{
		dir:      dir,
		maxBytes: maxBytes,
		sem:      make(chan struct{}, maxConcurrentDownloads),
		inflight: map[string]*cacheEntry{},
	}, nil
}

// cacheKey is built ONLY from things that identify the artifact: the endpoint name,
// the build ID and - for endpoints that have one - the rest of the path.
//
// r.RequestURI is deliberately NOT used. httprouter matches on r.URL.Path, so a
// query string reaches the handler without changing the response: "?n=1", "?n=2"...
// minted fresh keys, each one a separate upstream fetch and a separate blob on disk.
// A single client could fill the cache and multiply outbound traffic that way,
// without any privileges at all.
//
// extraPath is empty for debuginfo and executable; it exists so that adding
// "source" to cacheableEndpoints would not collapse every source file of one build
// ID under a single key.
//
// The key is logical (readable in logs); blobPath maps it onto a path.
func cacheKey(endpointName, buildID, extraPath string) string {
	return endpointName + "\x00" + buildID + "\x00" + extraPath
}

// isBlobName recognises the names blobPath generates: a sha256 in hex.
func isBlobName(name string) bool {
	if len(name) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// blobPath hashes the key ITSELF instead of assuming anything about its length or
// characters - otherwise a short key, or one containing "/", ends in a panic or an
// escape from the cache directory.
func (c *fileCache) blobPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(c.dir, h[0:2], h[2:4], h)
}

// Serve handles a request from the cache. It returns errors the same way the plain proxy path does.
func (c *fileCache) Serve(ctx context.Context, key string, w http.ResponseWriter, hostName string, fetch fetchFn) error {
	if meta, f, ok := c.openCached(key); ok {
		defer f.Close()
		return writeBody(w, f, meta, hostName, "HIT", false)
	}

	c.mu.Lock()
	e, exists := c.inflight[key]
	if exists {
		// The entry may already be dead: download marks done/err under e.mu and only
		// then removes the key under c.mu. Joining it inside that window would hand
		// the client somebody else's permanent error without a single byte of
		// network traffic.
		e.mu.Lock()
		finished := e.done
		e.mu.Unlock()
		if finished {
			exists = false
		}
	}
	if !exists {
		// The slot is taken BEFORE the entry is published, and NON-BLOCKING. The
		// semaphore used to live in download, i.e. after publication: with every slot
		// taken, the leader and everyone coalesced behind it waited 15 s in silence
		// and got a 500 - for a request the uncached path would have served fine.
		// Now a missing slot simply bypasses the cache.
		select {
		case c.sem <- struct{}{}:
		default:
			c.mu.Unlock()
			return ErrCacheBusy
		}
		e = newCacheEntry()
		c.inflight[key] = e
	}
	c.mu.Unlock()

	if !exists {
		// The leader starts the download in the BACKGROUND and then becomes an
		// ordinary follower itself. That way the leader disconnecting does not kill
		// the download for everyone else.
		go c.download(key, hostName, e, fetch)
	}

	status := "MISS"
	if exists {
		status = "COALESCED"
	}
	return c.follow(ctx, key, e, w, hostName, status)
}

// download runs in the background, detached from any client's context.
// finalErr is a NAMED return rather than a plain variable so a bare "return" cannot
// quietly report success. With a local variable, any new early exit that forgets to
// assign it ends in done=true with err=nil: followers get a clean io.EOF on a
// truncated file, i.e. a 200 with incomplete content.
func (c *fileCache) download(key, hostName string, e *cacheEntry, fetch fetchFn) (finalErr error) {
	logger := log.WithField("cache_key", key)
	// The slot was taken by Serve before the entry was published; it is returned here.
	defer func() { <-c.sem }()

	// tmpPath != "" means "this file still needs cleaning up"; the rename clears it
	// instead of setting a separate flag, so there are not two variables to keep in
	// sync.
	var (
		tmpPath      string
		bodyComplete bool // the whole body reached the file and the followers
	)
	// ONE defer instead of two: teardown order matters here and must not depend on
	// which line registered which defer (LIFO). First we take the descriptor away
	// from the followers and publish the outcome, and only then delete the file -
	// otherwise the tmp vanishes before anyone learns the real cause of the error.
	defer func() {
		e.mu.Lock()
		tmpToClose := e.tmpFile
		e.tmpFile = nil // from now on followers go for the finished blob
		e.done = true
		if e.err == nil && !bodyComplete {
			// Errors AFTER a complete body (Sync, writing .meta, rename) only mean
			// "could not store it in the cache" - the clients already have every
			// single byte. Recording them here would fail transfers that succeeded
			// and falsify error_msg in access_log.
			e.err = finalErr
		}
		e.cond.Broadcast()
		e.mu.Unlock()

		// Log ONLY after e.mu has been released. logrus writes synchronously, under
		// its own mutex, to stderr - and in a container stderr is a pipe to Docker's
		// log driver. When the reader stalls, the pipe buffer (64 KiB) fills and
		// write(2) BLOCKS. Under e.mu that would stall every follower of this entry,
		// and because Serve holds the global c.mu while waiting for e.mu, every
		// request for EVERY other build ID too - cache hits included. A blocked log
		// would turn into a stall of the whole service.
		//
		// The clients got everything, so finalErr does NOT go into e.err - but it
		// has to go somewhere. Without this, a write failure (ENOSPC, read-only,
		// wrong uid) means a permanently dead cache with zero signal: every request
		// re-downloads and the process emits not a single line.
		if bodyComplete && finalErr != nil {
			logger.WithError(finalErr).Error("cache: nie udalo sie utrwalic wpisu, tresc dostarczona")
		}

		c.mu.Lock()
		// Delete ONLY our own entry. Since Serve treats an entry with done=true as
		// dead and replaces it with a new one, an unconditional delete would remove
		// the SUCCESSOR - and then the next client finds nothing, creates a third
		// entry and starts a parallel download of the same artifact. The cascade eats
		// exactly the request coalescing that inflight exists for.
		if c.inflight[key] == e {
			delete(c.inflight, key)
		}
		c.mu.Unlock()

		if tmpToClose != nil {
			// Since we deliberately skip Sync, close(2) is the ONLY remaining moment
			// at which the kernel can still report a deferred write error - POSIX
			// permits this, and NFS, FUSE and overlay on a full medium do exactly
			// that (ENOSPC/EDQUOT/EIO only on close). On top of that this runs AFTER
			// the rename and after the .meta was written, so an ignored error leaves
			// a published entry with a truncated tail: openCached only compares the
			// inode's length with meta.Size, so it would pass on every hit, forever.
			if cerr := tmpToClose.Close(); cerr != nil && bodyComplete {
				// Only report it. Deleting the entry here would be worse than leaving
				// it:
				//  - the cause is unknown; a close(2) interrupted by a signal returns
				//    an error with the on-disk data intact,
				//  - delete(c.inflight, key) ran ABOVE, so the next request may
				//    already be serving this blob through the HIT path,
				//  - bodyComplete does not prove the blob under that name came from
				//    THIS download: Serve creates a new entry when the previous one
				//    has done=true, so two downloads of the same key can run in
				//    parallel and we would delete somebody else's correctly published
				//    file.
				//
				// To decide sensibly here we would need a way to verify the contents -
				// a checksum in the .meta, or comparing .note.gnu.build-id with the
				// key. Until then the write error is visible in the log, and an entry
				// of the wrong length is rejected by openCached anyway.
				logger.WithError(cerr).Error("cache: blad przy zamykaniu pliku po publikacji wpisu")
			}
		}
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), cacheFetchTimeout)
	defer cancel()

	resp, err := fetch(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	blob := c.blobPath(key)
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(blob), filepath.Base(blob)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath = tmp.Name() // cleaned up by the defer above if the rename never happened

	meta := cacheMeta{Status: resp.StatusCode, Host: hostName, Headers: map[string]string{}}
	for _, name := range proxyHeaders {
		if v := resp.Header.Get(name); v != "" {
			meta.Headers[name] = v
		}
	}

	// The cache must ALWAYS hold gzip - compression is a cost paid once on ingest,
	// and serving is meant to be verbatim. Cloudflare always sends us "br, gzip", so
	// without this a file from an upstream that ignores our Accept-Encoding would go
	// out uncompressed on EVERY request, for the rest of the entry's life.
	upstreamEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("content-encoding")))
	compressOnIngest := upstreamEncoding == "" || upstreamEncoding == "identity"
	if !compressOnIngest && upstreamEncoding != "gzip" {
		// We asked for gzip only, so this should not happen.
		// Store verbatim and restore the original header.
		logger.WithField("content_encoding", upstreamEncoding).
			Warn("cache: upstream zwrocil kodowanie, o ktore nie prosilismy")
	}

	// The chain: gzip -> bufio -> countingWriter -> file, but the buffer is used
	// ONLY when compressing on ingest.
	//
	// Why the buffer: deflate flushes its huffmanBitWriter roughly every 240 bytes,
	// so without it the file received a write(2) about every 240 B. Measured with
	// TLS-record-sized reads (16 KiB - the most crypto/tls.Conn.Read ever returns,
	// regardless of our buffer): 21,565 calls drop to 1,009, i.e. 21x fewer.
	//
	// Why NOT on the verbatim path: there is no fragmentation there - we write
	// exactly what came off the network, one chunk per read. A buffer the same size
	// as the read buffer, flushed after every chunk, coalesces nothing. Measured:
	// 1.00x at 16 KiB, 64 KiB and 256 KiB - not one syscall saved, at the price of
	// copying every byte and 256 KiB per slot (8 MiB across 32 downloads). And this
	// is the COMMON path: we ask upstreams for gzip, so a compliant server sends it
	// and compressOnIngest comes out false.
	//
	// The buffer is 32 KiB, not 256 KiB. A flush follows every network read, and
	// crypto/tls.Conn.Read returns at most one TLS record (16 KiB), so more than one
	// read can never fit inside it - the measured maximum handed to the file is
	// 16,394 B. 32 KiB yields an IDENTICAL write(2) count to 256 KiB (1,002) while
	// saving 7 MiB across 32 concurrent downloads.
	//
	// THE ORDER IS NOT ARBITRARY. The counter sits BELOW the buffer, so cw.n counts
	// bytes that actually reached the file - and cw.n is what we publish as e.size,
	// i.e. the promise "this much can be read via pread". With the counter above the
	// buffer, a forgotten flush would let e.size run ahead of the file and followers
	// would read past its end. In this order the worst consequence of a missed flush
	// is an e.size briefly too small, i.e. a follower waits.
	cw := &countingWriter{w: tmp}
	var dst io.Writer = cw
	var bw *bufio.Writer
	var zw *gzip.Writer
	if compressOnIngest {
		bw = bufio.NewWriterSize(cw, ingestBufSize)
		zw = gzip.NewWriter(bw)
		dst = zw
		// After compression the upstream's headers describe something other than the stored bytes.
		meta.Headers["content-encoding"] = "gzip"
		delete(meta.Headers, "content-length")
	}

	// publishSize closes EVERY buffering stage and only then announces the size.
	// One place, so the flush and the promise to followers are inseparable - with two
	// copies of this block, omitting the flush in one of them is an invisible typo no
	// test would catch.
	publishSize := func() error {
		if zw != nil {
			// Without this gzip.Writer would hold the data internally and cw.n would
			// not move, even though the bytes have already arrived.
			if err := zw.Flush(); err != nil {
				return err
			}
		}
		if bw != nil {
			if err := bw.Flush(); err != nil {
				return err
			}
		}
		e.mu.Lock()
		e.size = cw.n // the size ON DISK, not the number of bytes read
		e.cond.Broadcast()
		e.mu.Unlock()
		return nil
	}

	// From here on followers may inherit the descriptor and stream.
	// We expose the FILE, not the path: the rename below takes the path away but does
	// not touch the inode, so an inherited descriptor stays valid.
	//
	// meta is published as a COPY. cacheMeta.Headers is a map, so a copy of the
	// struct would still point at the same map this goroutine holds locally - adding
	// anything to it below (a Vary, say) would be a write to a map being read
	// concurrently by N followers in writeCachedHeaders, i.e. "fatal error:
	// concurrent map read and map write" and the death of the whole process.
	e.mu.Lock()
	e.tmpFile = tmp
	e.meta = meta.clone()
	e.headersReady = true
	e.cond.Broadcast()
	e.mu.Unlock()

	buf := make([]byte, cacheChunkSize)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if werr := publishSize(); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// A broken download must NOT enter the cache - otherwise we store a
			// truncated file and serve it until eviction.
			return rerr
		}
	}

	if zw != nil {
		// Close appends the gzip footer, so the size grows once more. That has to
		// reach the followers BEFORE the defer sets done, or they cut off just short
		// of the end of the stream.
		if err := zw.Close(); err != nil {
			return err
		}
	}
	// The final publish covers the gzip footer appended by the Close above and
	// whatever is left in the buffer. It has to reach the followers BEFORE the defer
	// sets done, or the stream cuts off just short of the end.
	if err := publishSize(); err != nil {
		return err
	}

	// From here the body is complete and delivered; any further error concerns only
	// persisting it in the cache.
	bodyComplete = true

	written := cw.n
	meta.Size = written
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// DELIBERATELY no tmp.Sync(). No follower sees EOF until the defer sets e.done,
	// and that defer waits for everything below - an fsync of 50 MiB is ~17 ms on
	// local NVMe and seconds on a network volume, charged to EVERY concurrent
	// client. The cache is reproducible: a crash costs one re-download, so a
	// durability barrier buys nothing here.
	// tmp is NOT closed here - the defer does that, after first taking the descriptor
	// away from the followers under e.mu. Closing at this point would invalidate an
	// fd a follower has just inherited. Renaming an open file is well defined.

	// The rename is atomic and does not touch the inode, so the followers' inherited
	// descriptors stay valid - there is no window in which the file disappears from
	// under them. Order: the blob first, then the .meta, with a rollback if the
	// second publish fails. The reverse order meant a FAILED refresh left a new .meta
	// beside the old blob - the sizes disagreed, so a working entry became a
	// permanent MISS because of a download that did not succeed.
	if err := os.Rename(tmpPath, blob); err != nil {
		return err
	}
	// The .meta atomically: os.WriteFile uses O_TRUNC, so it could truncate a live
	// file underneath a reading openCached.
	if err := writeFileAtomic(blob+".meta", metaBytes); err != nil {
		// A blob without metadata is unservable and invisible to size accounting -
		// better to delete the whole pair and re-download on the next request than to
		// leave litter nothing will ever clean up.
		os.Remove(blob)
		return err
	}
	tmpPath = "" // published under its final name, nothing left to clean up
	logger.WithField("bytes", written).Debug("cache: zapisano")
	return nil
}

// entryReader reads a file the leader is still appending to. Its whole substance is
// one thing: once it catches up with the leader, Read WAITS on the condvar instead
// of returning EOF - so io.Copy in writeBody does not end the response at the write
// head. A plain io.Reader in this position would hand the client truncated content
// with a 200.
type entryReader struct {
	ctx context.Context
	e   *cacheEntry
	f   *os.File
	off int64
}

func (r *entryReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		// Without this a zero-length read never enters the condvar wait (the leader is
		// ahead, so the predicate is false) and the loop would burn a full core until
		// the client disconnects.
		return 0, nil
	}
	for {
		// Read ONLY the range the leader has already published through e.size.
		// Without that bound we would reach into bytes currently being written, and
		// correctness would rest on unguaranteed filesystem behaviour - on NFS or
		// overlay a pread can see a partially materialised page, which during
		// transcoding ends in a gzip error AFTER the 200 header has gone out.
		r.e.mu.Lock()
		avail := r.e.size - r.off
		r.e.mu.Unlock()

		if avail > 0 {
			chunk := p
			if int64(len(chunk)) > avail {
				chunk = p[:avail]
			}
			// ReadAt, not Read: the descriptor is shared via dup, and pread takes the
			// offset explicitly - it neither uses nor modifies the shared file offset,
			// so followers do not interfere with each other or with the leader.
			// ReadAt signals EOF even when n > 0, hence the order of the checks.
			n, err := r.f.ReadAt(chunk, r.off)
			if n > 0 {
				r.off += int64(n)
				return n, nil
			}
			if err != nil && err != io.EOF {
				return 0, err
			}
			// avail > 0 but the read returned nothing: the filesystem has not caught
			// up with the leader's write (NFS/overlay). The condvar predicate is FALSE
			// here, so cond.Wait alone would not put us to sleep - hence the explicit,
			// short pause.
			//
			// But first we MUST check for the end of the download: without that a
			// follower would never see done or err, and once the entry is cleaned up
			// it would spin ~1000 times a second until the client disconnects, i.e.
			// for up to 60 minutes.
			r.e.mu.Lock()
			done, derr := r.e.done, r.e.err
			r.e.mu.Unlock()
			if derr != nil {
				return 0, derr
			}
			if done {
				// The download has finished and the file is still shorter than the
				// announced size - the content is incomplete and must not be reported
				// as a clean EOF.
				return 0, io.ErrUnexpectedEOF
			}

			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(time.Millisecond):
			}
			continue
		}

		r.e.mu.Lock()
		for r.e.size <= r.off && !r.e.done && r.ctx.Err() == nil {
			r.e.cond.Wait()
		}
		finished := r.e.done && r.e.size <= r.off
		derr := r.e.err
		r.e.mu.Unlock()

		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}
		if finished {
			if derr != nil {
				return 0, derr
			}
			return 0, io.EOF
		}
	}
}

// follow streams to the client a file the leader is still appending to.
// key is needed when the download finishes before we manage to inherit the
// descriptor - the content is then under its final name and we read it from there.
func (c *fileCache) follow(ctx context.Context, key string, e *cacheEntry, w http.ResponseWriter, hostName, cacheStatus string) error {
	// cond.Wait knows nothing about contexts, so we wake it by hand when the client
	// goes away - otherwise a disconnected follower would hang until the download
	// ends. AfterFunc registers a callback instead of parking a goroutine for the
	// whole response, and with a 60 min WriteTimeout and hundreds of followers that
	// difference is hundreds of live goroutines.
	defer context.AfterFunc(ctx, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.cond.Broadcast()
	})()

	e.mu.Lock()
	for !e.headersReady && !e.done && ctx.Err() == nil {
		e.cond.Wait()
	}
	if e.err != nil {
		err := e.err
		e.mu.Unlock()
		// There used to be a guard here turning the leader's 404 into a retryable
		// error when a follower had resolved a DIFFERENT host. Removed, because it did
		// not work and could not have: it wrapped the error with %w, so errors.Is in
		// the middleware still saw ErrDebuginfoNotFound and the response was a 404
		// anyway - the only difference was a longer error_msg. It was also practically
		// unreachable, because FindByBuildID pins a single host through
		// state.LastSuccess, so leader and follower almost always ask the same one.
		return err
	}
	if !e.headersReady {
		e.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The download finished without publishing headers and without an error - that
		// is our failure, not an upstream answer. It must NOT surface as
		// ErrDebuginfoNotFound: the middleware maps that to a 404, and a debuginfod
		// client and Cloudflare remember a 404 as a binding "the symbols do not
		// exist", when they do. A plain error yields 500, i.e. a retryable signal.
		return fmt.Errorf("cache: pobranie zakonczone bez naglowkow dla %q", key)
	}
	// The descriptor is inherited UNDER THE LOCK - the leader clears e.tmpFile under
	// that same lock just before closing its own, so either we get a valid copy in
	// time or we see nil and go for the finished blob. There is no third possibility.
	meta := e.meta
	var f *os.File
	var err error
	if e.tmpFile != nil {
		f, err = dupFile(e.tmpFile, c.blobPath(key))
	}
	e.mu.Unlock()

	if err != nil {
		return err
	}
	if f == nil {
		// The download finished before we got here: the content is already under its
		// final name. We keep OUR OWN cacheStatus - it was still a MISS or a
		// COALESCED, because we paid for that transfer. Writing "HIT" here would
		// inflate the hit rate by exactly the short downloads, i.e. by most of them.
		cachedMeta, cachedFile, ok := c.openCached(key)
		if !ok {
			// NOTE: e.err == nil does NOT mean "published" here. Errors after a
			// complete body (writing .meta, rename) are deliberately hidden from
			// followers, because those already received every byte - but we never
			// managed to take the descriptor and have nowhere to read the content
			// from.
			//
			// Deliberately NOT a 404: a missing cache entry is our problem, not a
			// "there are no symbols" answer that a debuginfod client and a CDN will
			// remember.
			return fmt.Errorf("cache: entry for %q was not published (disk write failed or evicted); retry", key)
		}
		defer cachedFile.Close()
		return writeBody(w, cachedFile, cachedMeta, hostName, cacheStatus, false)
	}
	defer f.Close()

	return writeBody(w, &entryReader{ctx: ctx, e: e, f: f}, meta, hostName, cacheStatus, true)
}

// writeBody sends the headers and the body. The cache ALWAYS holds gzip and it goes
// out exactly that way - no negotiation and no decompression on the fly.
//
// There used to be a transcode branch here: when a client did not advertise gzip,
// the blob was decompressed on every request. It cost 1.3 s of CPU per 400 MB hit
// (and on the COMMON path, because libdebuginfod does not advertise gzip), forced
// content-length to be dropped and with it chunked encoding - and turned one URL
// into two different representations with no Vary header, which in front of a CDN
// is a bug. Removed deliberately: all traffic arrives via Cloudflare, which settles
// the encoding with the client itself.
func writeBody(w http.ResponseWriter, body io.Reader, meta cacheMeta, hostName, cacheStatus string, streaming bool) error {
	writeCachedHeaders(w, meta, hostName, cacheStatus, streaming)
	w.WriteHeader(meta.Status)
	_, err := io.Copy(w, body)
	return err
}

func (c *fileCache) openCached(key string) (cacheMeta, *os.File, bool) {
	blob := c.blobPath(key)

	// discard - the entry is corrupt or unreadable, rather than simply absent. We
	// report it and treat it as a miss. Nothing is deleted, DELIBERATELY:
	//
	//  1. Deleting buys nothing. A corrupt entry heals itself, because download()
	//     publishes via os.Rename, which overwrites an existing file
	//     unconditionally - and so does writeFileAtomic for the .meta. Verified:
	//     planting garbage under a blob's name and issuing two requests yields
	//     correct content and x-cache=HIT, without a single deletion.
	//  2. Nor does it linger on disk. os.Chtimes sits at the end of this function,
	//     past all the validation, so a corrupt entry NEVER refreshes its mtime -
	//     which makes it the oldest in the directory, i.e. eviction's first victim
	//     rather than its last.
	//  3. And it could cost everything. An error on this path need say nothing about
	//     the file: EMFILE, EACCES, EIO or ESTALE describe the process or the mount.
	//     EMFILE in particular is a PROCESS-WIDE condition, so deleting would mean
	//     that on descriptor exhaustion every request deletes exactly the blob it is
	//     asking for - at the rate of traffic. For an archive whose upstream no
	//     longer has those symbols, that loss is permanent.
	//
	// The only real fault here was that these cases left NO trace at all and were
	// indistinguishable from a cold cache. The log fixes that.
	discard := func(reason string, err error) (cacheMeta, *os.File, bool) {
		log.WithError(err).WithField("blob", filepath.Base(blob)).
			WithField("reason", reason).Warn("cache: wpis nie do uzycia, traktuje jak chybienie")
		return cacheMeta{}, nil, false
	}

	metaBytes, err := os.ReadFile(blob + ".meta")
	if err != nil {
		// A missing .meta is the ONLY normal case - an ordinary miss. Stay silent.
		// This also covers the window between renaming the blob and writing the .meta
		// in download().
		if os.IsNotExist(err) {
			return cacheMeta{}, nil, false
		}
		return discard("meta nieczytelne", err)
	}
	var meta cacheMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return discard("meta nie jest poprawnym JSON-em", err)
	}
	// The status goes straight into w.WriteHeader, and net/http panics on a value
	// outside the valid range. A .meta left by an older version, by a crash, or found
	// in someone else's directory may have none at all (=0), so the contents of the
	// disk are not trusted.
	if meta.Status < 100 || meta.Status > 999 {
		return discard(fmt.Sprintf("status %d poza zakresem", meta.Status), nil)
	}
	f, err := os.Open(blob)
	if err != nil {
		if os.IsNotExist(err) {
			// A .meta without its blob. This is the ONLY case where we delete - and
			// we may, because an orphaned .meta is metadata with no data behind it:
			// there is nothing here to lose. Without this we would emit a warning on
			// EVERY request for that build ID, forever: eviction will not clean the
			// .meta up (it skips it during the scan and removes it only alongside its
			// blob), and the log is synchronous, so it would run at the rate of
			// traffic.
			os.Remove(blob + ".meta")
			return cacheMeta{}, nil, false
		}
		return discard("blob nieczytelny", err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != meta.Size {
		// A size mismatch means a truncated file or a stale meta. Do not serve it.
		f.Close()
		return discard("rozmiar bloba nie zgadza sie z meta.Size", err)
	}
	// The mtime doubles as the LRU marker for eviction.
	now := time.Now()
	os.Chtimes(blob, now, now)
	return meta, f, true
}

func writeCachedHeaders(w http.ResponseWriter, meta cacheMeta, hostName, cacheStatus string, streaming bool) {
	h := w.Header()
	for _, name := range proxyHeaders {
		h.Del(name)
		if v, ok := meta.Headers[name]; ok && v != "" {
			h.Set(name, v)
		}
	}
	if streaming {
		// The final size is not known yet (with compression on ingest nobody knows
		// it). The upstream's header describes something other than what we will
		// send.
		h.Del("content-length")
	} else {
		// From disk: the exact size is known, even if the upstream went out chunked.
		// The mode comes from the PARAMETER, not from meta.Size > 0 - a zero-sized
		// blob is legal and under that condition would be taken for an in-flight
		// download forever.
		h.Set("content-length", strconv.FormatInt(meta.Size, 10))
	}
	// x-server names the host that PRODUCED these bytes, not whichever one the
	// resolver picked for this request. An old .meta (from before the field existed)
	// has none recorded - the value from the current resolution stands in then.
	if meta.Host != "" {
		h.Set("x-server", meta.Host)
	} else {
		h.Set("x-server", hostName)
	}
	h.Set("x-cache", cacheStatus)
}

// EvictLoop keeps the directory size in check; it deletes the least recently used entries.
func (c *fileCache) EvictLoop(ctx context.Context) {
	ticker := time.NewTicker(cacheEvictPeriod)
	defer ticker.Stop()
	for {
		if err := c.evictOnce(); err != nil {
			log.WithError(err).Error("cache: eviction err")
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

type cacheFileInfo struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *fileCache) evictOnce() error {
	if c.maxBytes <= 0 {
		return nil
	}

	var files []cacheFileInfo
	var total int64
	var skipped int
	var tmpRemoved int
	var tmpFreed int64
	err := filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// The root is not "a single entry" - if c.dir cannot be read, the whole
			// scan is worthless and total would come out zero. Return the error so
			// EvictLoop has something to log, instead of repeatedly reporting an empty
			// cache.
			if path == c.dir {
				return err
			}
			// An entry vanishing between readdir and lstat is normal work, not a
			// fault: download() creates and removes .tmp-* in exactly these shards.
			// The name filter below only runs when err == nil, so without this
			// carve-out the warning would fire on most cycles of a busy proxy.
			if os.IsNotExist(err) {
				return nil //nolint:nilerr // the file vanished during the scan, nothing happened
			}
			// Below: an unreadable entry does not abort the scan, but it MUST be
			// counted. A skipped subdirectory (wrong uid after a redeploy, EIO on a
			// shard) understates total, and then evictOnce sees "we fit in the budget"
			// and deletes nothing while the volume keeps filling.
			skipped++
			return nil //nolint:nilerr // counted above, the scan continues
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.Contains(base, ".tmp-") {
			// An unfinished download. The defer in download() used to clean these up,
			// but on SIGKILL - and `docker rm -f` in run.sh is exactly a SIGKILL - it
			// never gets the chance. Until now such files were invisible to the budget
			// AND unremovable: eviction skipped them in both roles at once. On a
			// persistent volume that meant the gap between CACHE_MAX_BYTES and actual
			// occupancy grew by whatever was downloading at every deploy.
			//
			// An age threshold rather than blind deletion: a younger .tmp may belong
			// to a download running RIGHT NOW - ours, or one from an old container
			// that has not exited yet. Nothing can outlive cacheFetchTimeout.
			if time.Since(info.ModTime()) > cacheFetchTimeout {
				if rerr := os.Remove(path); rerr == nil {
					tmpRemoved++
					tmpFreed += info.Size()
				}
				return nil
			}
			// A live download really does occupy space, so it counts towards the budget.
			total += info.Size()
			return nil
		}
		if strings.HasSuffix(path, ".meta") {
			// Not a deletion candidate (it disappears with its blob), but it does
			// occupy space - let total describe what is actually on disk.
			total += info.Size()
			return nil
		}
		// Delete ONLY files shaped like our blobs (64 hex characters). CACHE_PATH is
		// given by the operator and may point at a shared directory or one that
		// already holds data - without this filter eviction would remove other
		// people's files, starting with the oldest, i.e. with the ones it certainly
		// did not create.
		if !isBlobName(filepath.Base(path)) {
			return nil
		}
		files = append(files, cacheFileInfo{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	// The warning comes BEFORE the threshold check: it is precisely with an
	// understated total that the early return below is falsely reassuring, so the
	// signal must not depend on whether we happen to be over budget.
	if tmpRemoved > 0 {
		log.WithField("removed", tmpRemoved).WithField("freed_bytes", tmpFreed).
			Info("cache: sprzatnieto porzucone pliki tymczasowe")
	}
	if skipped > 0 {
		log.WithField("skipped", skipped).WithField("total_bytes", total).
			Warn("cache: eviction pominal nieczytelne wpisy, total jest zanizony")
	}
	if total <= c.maxBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	target := int64(float64(c.maxBytes) * cacheEvictTarget)
	var freed int64
	var removed int
	for _, f := range files {
		if total-freed <= target {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		os.Remove(f.path + ".meta")
		freed += f.size
		removed++
	}
	log.WithField("removed", removed).WithField("freed_bytes", freed).
		WithField("total_bytes", total).Info("cache: eviction")
	return nil
}
