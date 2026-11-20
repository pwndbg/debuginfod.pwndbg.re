// Package pkgarchive reads the file tree out of a package archive as a stream.
//
// Two commands need exactly the same thing from an archive and nothing more:
// cmd/indexer walks every member once to record build IDs, and
// cmd/index-debuginfod walks to one member to serve it. Both read straight from
// an HTTP body - nothing is written to disk - so everything here is forward-only.
//
// A .deb is an ar container whose data.tar.* holds the files; the other formats
// are a compressed tar directly. Either way the caller gets a *tar.Reader.
package pkgarchive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Formats, as stored in the index. They name the packing, not the content.
const (
	Deb    = "deb"
	TarGz  = "tar.gz"
	TarZst = "tar.zst"
	TarXz  = "tar.xz"
)

// FormatOf derives the format from a file name. The names this is used on are
// produced by release pipelines and archive tooling, never typed by a user, so
// the extension is reliable.
func FormatOf(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, ".deb"), strings.HasSuffix(name, ".ddeb"):
		return Deb, true
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return TarGz, true
	case strings.HasSuffix(name, ".tar.zst"):
		return TarZst, true
	case strings.HasSuffix(name, ".tar.xz"):
		return TarXz, true
	}
	return "", false
}

// Tar returns a reader over the archive's files. close releases decoder state -
// the zstd decoder owns goroutines, and a process that opens one archive per
// request would leak a set each time - and must be called once done.
func Tar(format string, body io.Reader) (tr *tar.Reader, close func(), err error) {
	if format == Deb {
		data, name, err := debData(body)
		if err != nil {
			return nil, nil, err
		}
		return tarOf(strings.TrimPrefix(name, "data."), data)
	}
	return tarOf(format, body)
}

func tarOf(format string, body io.Reader) (*tar.Reader, func(), error) {
	switch format {
	case "tar":
		return tar.NewReader(body), func() {}, nil
	case TarGz:
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, nil, err
		}
		return tar.NewReader(gz), func() { gz.Close() }, nil
	case TarZst:
		zr, err := zstd.NewReader(body)
		if err != nil {
			return nil, nil, err
		}
		return tar.NewReader(zr), zr.Close, nil
	case TarXz:
		xr, err := xz.NewReader(bufio.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		return tar.NewReader(xr), func() {}, nil
	}
	return nil, nil, fmt.Errorf("unsupported archive format %q", format)
}

// ErrNoData is returned for an ar archive with no data.tar member - not a
// binary package at all.
var ErrNoData = errors.New("no data.tar member in .deb")

const (
	arMagic     = "!<arch>\n"
	arHeaderLen = 60
)

// debData walks the ar container up to data.tar.* and returns a reader over
// exactly that member, plus its name (data.tar.zst, data.tar.xz, ...).
//
// Hand-rolled because the format is a 60-byte fixed header per member and the
// standard library has no reader for it. Debian puts debian-binary and
// control.tar.* first, so reaching data costs skipping a few kilobytes.
func debData(r io.Reader) (io.Reader, string, error) {
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, "", fmt.Errorf("reading ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, "", errors.New("not an ar archive")
	}
	hdr := make([]byte, arHeaderLen)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, "", ErrNoData
			}
			return nil, "", fmt.Errorf("reading ar header: %w", err)
		}
		if !bytes.Equal(hdr[58:60], []byte("`\n")) {
			return nil, "", errors.New("corrupt ar header")
		}
		// GNU ar terminates names with '/'; BSD-style and dpkg's own do not.
		name := strings.TrimRight(strings.TrimSpace(string(hdr[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, "", fmt.Errorf("bad ar member size %q", hdr[48:58])
		}
		if strings.HasPrefix(name, "data.tar") {
			return io.LimitReader(r, size), name, nil
		}
		// Members are padded to an even length.
		if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
			return nil, "", fmt.Errorf("skipping ar member %s: %w", name, err)
		}
	}
}

// Member normalises a tar entry name to the form stored in the index: no
// leading "./", which dpkg-deb adds and release tarballs do not.
func Member(name string) string {
	return strings.TrimPrefix(name, "./")
}
