package nix

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/nix-community/go-nix/pkg/narinfo"
	"github.com/pwndbg/debuginfod.pwndbg.re/useragent"
	"github.com/sirupsen/logrus"
	"github.com/ulikunitz/xz"
	"golang.org/x/sync/errgroup"
)

// NixDebuginfo is exported because cmd/nix-debuginfod has to hold one in a struct
// field; an unexported return type can only be used through inference, which
// stops working the moment the caller needs to name it.
type NixDebuginfo struct {
	cachePath        string
	nixSubstituteUrl url.URL
	logger           *logrus.Entry
	client           *http.Client
}

var ErrNixDebuginfoNotFound = errors.New("nix debuginfo not found")

func NewNixDebuginfo(logger *logrus.Entry) *NixDebuginfo {
	// Every fetch below does logger.WithField(...). A nil *logrus.Entry panics
	// there, which turned the first request into a crash rather than an error.
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	// TODO: cachePath moze nie chcemy takiego
	cachePath, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}
	cachePath = path.Join(cachePath, "nix-debuginfod")
	if err := os.MkdirAll(cachePath, 0755); err != nil {
		log.Fatal(err)
	}

	// todo: url
	u2, err := url.Parse("https://cache.nixos.org")
	if err != nil {
		log.Fatal(err)
	}

	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		log.Fatal(fmt.Errorf("`mkfs.erofs` not found in $PATH, it is required"))
	}

	return &NixDebuginfo{
		cachePath:        cachePath,
		nixSubstituteUrl: *u2,
		logger:           logger,
		// Wrapped so every request to cache.nixos.org identifies us: this used
		// to be http.DefaultClient and went out as Go-http-client/2.0, with no
		// way for the nix infrastructure to tell whose traffic it was.
		// TODO: moze trzeba inne limity na timout na nagłówki ustawic lub inne limity
		client: useragent.Client(http.DefaultClient, "nix"),
	}
}

type debuginfoJsonResponse struct {
	ArchiveRelative string `json:"archive"`
	Member          string `json:"member"`
}

func (s *NixDebuginfo) getNixRemoteUrl(elems ...string) string {
	u := s.nixSubstituteUrl
	u.Path = path.Clean(path.Join(elems...))
	return u.String()
}

func (s *NixDebuginfo) fetchDebuginfo(ctx context.Context, buildID string) (*debuginfoJsonResponse, error) {
	logger := s.logger.WithField("buildid", buildID)
	logger.Debug("started fetching debuginfo")

	req, err := http.NewRequestWithContext(ctx, "GET", s.getNixRemoteUrl("debuginfo", buildID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	logger.WithError(err).Debug("finished fetching debuginfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNixDebuginfoNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nix debuginfo returned status %d", resp.StatusCode)
	}

	var out debuginfoJsonResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	// remove "../" from first path
	out.ArchiveRelative = strings.TrimPrefix(out.ArchiveRelative, "../")
	return &out, nil
}

func (s *NixDebuginfo) fetchNar(ctx context.Context, relativePath string) (io.ReadCloser, error) {
	logger := s.logger.WithField("relativePath", relativePath)
	logger.Debug("started fetching nar")

	req, err := http.NewRequestWithContext(ctx, "GET", s.getNixRemoteUrl(relativePath), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	logger.WithError(err).Debug("finished fetching nar")
	if err != nil {
		return nil, err
	}

	// No plain defer here: on success the body is returned to the caller, still
	// open. Every error path below therefore has to close it by hand - without
	// this a 404, which is the common case, leaked a connection per request.
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNixDebuginfoNotFound
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("nix nar returned status %d", resp.StatusCode)
	}

	// The format is sniffed rather than taken from the narinfo's Compression
	// field, because the two entry points disagree about which one there even is:
	// /debuginfo/<buildid> hands back an ArchiveRelative with no narinfo at all,
	// while FetchNarByStorePath has one. Sniffing covers both with one rule.
	//
	// zstd is not optional. cache.nixos.org serves .nar.zst today - every narinfo
	// fetched through it says `Compression: zstd` - while the /debuginfo archive
	// paths are still .nar.xz. Assuming xz for anything that was not a raw NAR
	// therefore worked for debuginfo and failed for executable and source with
	// "xz: invalid header magic bytes".
	var (
		narMagic  = []byte("\x0d\x00\x00\x00\x00\x00\x00\x00nix-archive-1")
		xzMagic   = []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}
		zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}
	)

	head := make([]byte, len(narMagic))
	if _, err := io.ReadAtLeast(resp.Body, head, len(head)); err != nil {
		resp.Body.Close()
		return nil, err
	}
	// Put the bytes back: nothing below may assume the stream is still at 0.
	body := io.MultiReader(bytes.NewReader(head), resp.Body)

	switch {
	case bytes.Equal(head, narMagic):
		return readCloser{Reader: body, closerFunc: resp.Body.Close}, nil

	case bytes.HasPrefix(head, zstdMagic):
		dec, err := zstd.NewReader(bufio.NewReader(body))
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("zstd decompressor: %w", err)
		}
		return readCloser{
			Reader: dec.IOReadCloser(),
			closerFunc: func() error {
				dec.Close()
				return resp.Body.Close()
			},
		}, nil

	case bytes.HasPrefix(head, xzMagic):
		dec, err := xz.NewReader(bufio.NewReader(body)) // xz+bufio = speed
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("xz decompressor: %w", err)
		}
		return readCloser{Reader: dec, closerFunc: resp.Body.Close}, nil
	}

	resp.Body.Close()
	return nil, fmt.Errorf("unrecognised nar encoding for %s (first bytes %x)", relativePath, head[:8])
}

func (s *NixDebuginfo) fetchNarInfo(ctx context.Context, storeHash string) (*narinfo.NarInfo, error) {
	logger := s.logger.WithField("storeHash", storeHash)
	logger.Debug("started fetching narinfo")

	req, err := http.NewRequestWithContext(ctx, "GET", s.getNixRemoteUrl(storeHash+".narinfo"), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	logger.WithError(err).Debug("finished fetching narinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNixDebuginfoNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nix narinfo returned status %d", resp.StatusCode)
	}

	return narinfo.Parse(resp.Body)
}

// DebuginfoMeta looks up a build ID and returns the archive holding its debug
// info plus the member path inside it.
//
// Split out from the image build on purpose. This half is a 159-byte JSON fetch
// and is per build ID; the other half downloads and repacks a NAR that is
// *shared* - one glibc archive serves 389 build IDs - so keying the expensive
// half by build ID meant downloading the same 11.8 MB archive 389 times.
func (s *NixDebuginfo) DebuginfoMeta(ctx context.Context, buildid string) (archive, member string, err error) {
	resp, err := s.fetchDebuginfo(ctx, buildid)
	if err != nil {
		return "", "", err
	}
	return resp.ArchiveRelative, resp.Member, nil
}

// FetchNarByArchive builds an erofs image at outFile from a NAR named by its
// path in the binary cache. The caller keys the result by that archive, so build
// IDs sharing one download it once.
func (s *NixDebuginfo) FetchNarByArchive(ctx context.Context, archive, outFile string) error {
	g, ctx := errgroup.WithContext(ctx)

	reader, err := s.fetchNar(ctx, archive)
	if err != nil {
		return err
	}
	defer reader.Close()

	pipeR, pipeW := io.Pipe()
	g.Go(func() error {
		defer pipeW.Close()
		writer := bufio.NewWriter(pipeW)
		defer writer.Flush()
		return NarUnpackerAsTarball(ctx, reader, writer, "")
	})
	g.Go(func() error {
		defer pipeR.Close()
		return TarballToErofs(ctx, bufio.NewReader(pipeR), outFile)
	})

	return g.Wait()
}

func (s *NixDebuginfo) FetchNarByStorePath(ctx context.Context, storePath string, outFile string) error {
	// Performance speed: xz decompression+tar+erofs ~1GB in 60s
	g, ctx := errgroup.WithContext(ctx)

	_, storeHash, filename := NixStorePathToHash(storePath)
	resp, err := s.fetchNarInfo(ctx, storeHash)
	if err != nil {
		return err
	}

	reader, err := s.fetchNar(ctx, resp.URL)
	if err != nil {
		return err
	}
	defer reader.Close()

	pipeR, pipeW := io.Pipe()
	g.Go(func() error {
		defer pipeW.Close()
		writer := bufio.NewWriter(pipeW)
		defer writer.Flush()
		return NarUnpackerAsTarball(ctx, reader, writer, filename)
	})
	g.Go(func() error {
		defer pipeR.Close()
		return TarballToErofs(ctx, bufio.NewReader(pipeR), outFile)
	})

	if err := g.Wait(); err != nil {
		return err
	}
	return nil
}
