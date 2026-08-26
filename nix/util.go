package nix

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path"
	"strings"

	"github.com/nix-community/go-nix/pkg/nar"
	"github.com/ulikunitz/xz"
)

func NarUnpackerAsTarball(ctx context.Context, r io.Reader, w io.Writer, filename string) error {
	nr, err := nar.NewReader(bufio.NewReader(r))
	if err != nil {
		return err
	}
	defer nr.Close()

	hdrRoot, err := nr.Next()
	if err == io.EOF {
		return errors.New("NarUnpackerAsTarball: no nar file")
	} else if err != nil {
		return fmt.Errorf("NarUnpackerAsTarball: reading root header: %w", err)
	}
	if hdrRoot.Path != "/" {
		return fmt.Errorf("NarUnpackerAsTarball: incorrect nar root file path: %s", hdrRoot.Path)
	}

	switch hdrRoot.Type {
	case nar.TypeRegular:
		return unpackNarFileToTarball(ctx, nr, w, filename)
	case nar.TypeDirectory:
		return unpackNarDirectoryToTarball(ctx, nr, w, filename)
	}
	return fmt.Errorf("NarUnpackerAsTarball: nar root file type not supported: %s", hdrRoot.Type)
}

func unpackNarDirectoryToTarball(ctx context.Context, nr *nar.Reader, w io.Writer, filename string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := nr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("unpackNarDirectoryToTarball: next err: %w", err)
		}

		tarHeader := &tar.Header{
			Name:     hdr.Path[1:],
			Linkname: hdr.LinkTarget,
			Size:     hdr.Size,
			Mode:     0o444, // read only
		}
		if hdr.Executable {
			tarHeader.Mode |= 0o111 // exec
		}

		switch hdr.Type {
		case nar.TypeRegular:
			tarHeader.Typeflag = tar.TypeReg
		case nar.TypeSymlink:
			tarHeader.Typeflag = tar.TypeSymlink
		case nar.TypeDirectory:
			tarHeader.Typeflag = tar.TypeDir
			tarHeader.Name += "/"
		default:
			return fmt.Errorf("unpackNarDirectoryToTarball: invalid nar type: %v", hdr.Type)
		}
		if err := tw.WriteHeader(tarHeader); err != nil {
			return fmt.Errorf("unpackNarDirectoryToTarball: writing tar header, file=%s: %w", hdr.Path, err)
		}
		if written, err := io.Copy(tw, nr); err != nil {
			return fmt.Errorf("unpackNarDirectoryToTarball: writing tar contents, file=%s: %w", hdr.Path, err)
		} else if written != hdr.Size {
			return fmt.Errorf("unpackNarDirectoryToTarball: expected to write %d bytes, wrote %d", hdr.Size, written)
		}
	}
	return nil
}

func unpackNarFileToTarball(ctx context.Context, r io.Reader, w io.Writer, filename string) error {
	var (
		decompressed io.Reader
		err          error
	)

	switch detectCompression(filename) {
	case "xz":
		decompressed, err = xz.NewReader(bufio.NewReader(r)) // xz+bufio = speed
		if err != nil {
			return fmt.Errorf("unpackNarFileToTarball: xz decompressor: %w", err)
		}
	case "bzip2":
		decompressed = bzip2.NewReader(r)
	case "gzip":
		tmp, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("unpackNarFileToTarball: gzip decompressor: %w", err)
		}
		decompressed = tmp
		defer tmp.Close()
	default:
		// maybe tarfile, zipfile, only other binary/text file
		// FIXME: add zipfile support or more file types
		decompressed = r
	}

	decompressed, err = isTarball(decompressed)
	if err != nil {
		return fmt.Errorf("unpackNarFileToTarball: not a tar, file=%s: %w", filename, err)
	}

	if _, err := io.Copy(w, decompressed); err != nil {
		return fmt.Errorf("unpackNarFileToTarball: writing tar contents, file=%s: %w", filename, err)
	}
	return nil
}

func isTarball(r io.Reader) (io.Reader, error) {
	w := bytes.NewBuffer(nil) // FIXME: can buf explode memory?
	tr := tar.NewReader(io.TeeReader(r, w))
	_, err := tr.Next()
	revertedReader := io.MultiReader(bytes.NewReader(w.Bytes()), r)

	if err != nil && err != io.EOF {
		return revertedReader, err
	}
	return revertedReader, nil
}

func detectCompression(filename string) string {
	// Similar logic from func `_defaultUnpack`
	// from nixpkgs/pkgs/stdenv/generic/setup.sh
	// commit cb82756ecc37fa623f8cf3e88854f9bf7f64af93

	lowerFilename := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lowerFilename, ".tar.xz"),
		strings.HasSuffix(lowerFilename, ".tar.lzma"),
		strings.HasSuffix(lowerFilename, ".txz"):
		return "xz"
	case strings.HasSuffix(lowerFilename, ".tar.gz"),
		strings.HasSuffix(lowerFilename, ".tgz"),
		strings.HasSuffix(lowerFilename, ".tar.Z"):
		return "gzip"
	case strings.HasSuffix(lowerFilename, ".tar.bz2"),
		strings.HasSuffix(lowerFilename, ".tbz2"),
		strings.HasSuffix(lowerFilename, ".txz"):
		return "bzip2"
	}
	return "none"
}

func TarballToErofs(ctx context.Context, r io.Reader, outputPath string) error {
	// Measured on a real source tree (Go stdlib: 150 MB, 11476 files, median file
	// 1766 B), which is the shape that matters here - /source/* is thousands of
	// small files per build ID:
	//
	//	-Enoinline_data, no -z   151.8 MB   <- what this used to build
	//	-zlz4hc, inline allowed   58.8 MB   <- 2.58x smaller
	//
	// Both flags changed, for separate reasons.
	//
	// -zlz4hc, and lz4 specifically, because of a two-sided constraint that is
	// invisible from either side alone:
	//
	//	our mkfs.erofs (alpine erofs-utils 1.9) offers lz4, lz4hc, deflate
	//	the deploy host's kernel has ZIP=y (lz4) and ZIP_LZMA=y, but NOT
	//	  ZIP_DEFLATE and NOT ZIP_ZSTD
	//
	// So -zdeflate builds cleanly here and then cannot be mounted there - failing
	// in production only - while -zlzma would mount there but mkfs cannot produce
	// it. The intersection is lz4/lz4hc. Do not change this without checking
	// `zgrep EROFS /proc/config.gz` on the host. It is also the better trade on
	// merit: this is the read path for every source file, an image is read many
	// times after being built once, and lz4 decompresses far faster than lzma.
	//
	// -Enoinline_data is gone. It kept small files' data out of the inode, which
	// costs a whole 4 KB block per file and added 22 MB (+36%) to the tree above.
	// It was presumably a workaround for go-erofs; the kernel reads these images
	// now, and a mounted inline-data image was verified to read back all 11476
	// files byte-identically.
	args := []string{"--tar=f", "--quiet", "-zlz4hc"} // TODO: replace `-zlz4hc` with `-Enoinline_data` maybe
	args = append(args, outputPath)
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	cmd.Stdin = r
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("TarballToErofs: mkfs.erofs failed: %s: %w", out, err)
	}
	return nil
}

type readCloser struct {
	io.Reader
	closerFunc func() error
}

func (rc readCloser) Close() error {
	return rc.closerFunc()
}

func NixStorePathToHash(originalPath string) (string, string, string) {
	filename := path.Base(originalPath)
	idx := strings.IndexByte(filename, '-')
	if idx == -1 {
		log.Fatalf("invalid store path %q, never should happen", originalPath)
	}
	storeHash := strings.ToLower(filename[:idx]) // GCC sometimes put uppercase in ELF's?
	fileName := filename[idx+1:]
	fixedPath := path.Join(path.Dir(originalPath), storeHash+"-"+fileName)
	return fixedPath, storeHash, fileName
}
