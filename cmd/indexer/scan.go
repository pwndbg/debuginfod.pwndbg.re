package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/pwndbg/debuginfod.pwndbg.re/pkgarchive"
)

// Artifact is one ELF file inside an archive, keyed by its build id.
type Artifact struct {
	BuildID string `json:"buildid"`
	// Member is the path inside the archive's file tree, e.g.
	// python/bin/python3.14 or usr/lib/debug/.build-id/98/51d7....debug. A
	// serving backend re-fetches the archive and streams this entry out.
	Member string `json:"member"`
	Size   int64  `json:"size"`
}

// errGone marks an archive the server no longer has. Recorded in the resume
// log as done rather than failed, so an expired Launchpad file is not
// re-requested on every run.
var errGone = errors.New("archive is gone (404)")

// ScanArchive streams one archive and returns every build id in it.
//
// The archive is never written to disk. An install_only asset is ~126 MB
// compressed and expands to well over a gigabyte, and the scan only needs the
// notes of each ELF - so it decompresses through and discards, keeping memory
// flat and costing one pass instead of a download plus an extraction.
func ScanArchive(ctx context.Context, hc *http.Client, a Archive) ([]Artifact, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	// The archive's own bytes, never a transparently decoded copy: Launchpad's
	// librarian sends .gz files with Content-Encoding: gzip, and Go's transport
	// would strip the very compression a.Format says to expect.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errGone
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", a.URL, resp.Status)
	}
	return scanFormat(a.Format, resp.Body)
}

// ScanTarball scans an archive whose format follows from its file name.
func ScanTarball(name string, body io.Reader) ([]Artifact, error) {
	format, ok := pkgarchive.FormatOf(name)
	if !ok {
		return nil, fmt.Errorf("unsupported archive: %s", name)
	}
	return scanFormat(format, body)
}

// debugPath matches the layout every distribution uses for separate debug
// files. The build id is spelled out in the path, split after two characters.
var debugPath = regexp.MustCompile(`\.build-id/([0-9a-f]{2})/([0-9a-f]{6,})\.debug$`)

func scanFormat(format string, body io.Reader) ([]Artifact, error) {
	tr, closeFn, err := pkgarchive.Tar(format, body)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	var out []Artifact
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			// Symlinks in these archives point within the same archive
			// (python3 -> python3.14), so the target is indexed under its own
			// name anyway and the link adds nothing.
			continue
		}
		member := pkgarchive.Member(hdr.Name)

		id, err := BuildIDFromELF(tr, hdr.Size)
		if err != nil {
			// The ELF note is the authority, but a separate debug file names
			// its build id in its path as well. Falling back to that covers a
			// .debug whose program headers objcopy left pointing at nothing.
			if m := debugPath.FindStringSubmatch(member); m != nil {
				id = m[1] + m[2]
			} else {
				// Most of a tarball is .py files and data; not being an ELF is
				// the normal case, not a problem. A relocatable .o has no note.
				if !errors.Is(err, errNotELF) && !errors.Is(err, ErrNoBuildID) {
					log.WithError(err).WithField("member", member).Debug("skipping unreadable ELF")
				}
				continue
			}
		}

		if seen[id+"\x00"+member] {
			continue
		}
		seen[id+"\x00"+member] = true
		out = append(out, Artifact{BuildID: id, Member: member, Size: hdr.Size})
	}
}
