package pkgarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

func compress(t *testing.T, format string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch format {
	case TarGz:
		w = gzip.NewWriter(&buf)
	case TarZst:
		w, err = zstd.NewWriter(&buf)
	case TarXz:
		w, err = xz.NewWriter(&buf)
	case "tar":
		return raw
	}
	if err != nil {
		t.Fatal(err)
	}
	w.Write(raw)
	w.Close()
	return buf.Bytes()
}

// arMember writes one ar member the way dpkg-deb does, padding odd sizes.
func arMember(buf *bytes.Buffer, name string, body []byte) {
	fmt.Fprintf(buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
	buf.Write(body)
	if len(body)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func deb(t *testing.T, dataName string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	// An odd-length control member, so a reader that forgets the padding
	// lands one byte short of the next header.
	arMember(&buf, "control.tar.xz", []byte("abc"))
	arMember(&buf, dataName, data)
	return buf.Bytes()
}

func readAll(t *testing.T, tr *tar.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[Member(hdr.Name)] = string(b)
	}
}

func TestDebDataInEveryCompression(t *testing.T) {
	files := map[string]string{"./usr/lib/debug/.build-id/98/51d7.debug": "ELF"}
	for _, c := range []struct{ member, format string }{
		{"data.tar.zst", TarZst}, {"data.tar.xz", TarXz}, {"data.tar.gz", TarGz}, {"data.tar", "tar"},
	} {
		t.Run(c.member, func(t *testing.T) {
			body := deb(t, c.member, compress(t, c.format, tarBytes(t, files)))
			tr, closeFn, err := Tar(Deb, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer closeFn()
			got := readAll(t, tr)
			if got["usr/lib/debug/.build-id/98/51d7.debug"] != "ELF" {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestDebWithoutDataIsAnError(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	if _, _, err := Tar(Deb, &buf); !errors.Is(err, ErrNoData) {
		t.Fatalf("want ErrNoData, got %v", err)
	}
	if _, _, err := Tar(Deb, bytes.NewReader([]byte("<html>404</html>"))); err == nil {
		t.Fatal("an HTML error page must not read as a .deb")
	}
}

func TestPlainTarballs(t *testing.T) {
	files := map[string]string{"python/bin/python3.14": "ELF"}
	for _, f := range []string{TarGz, TarZst, TarXz} {
		tr, closeFn, err := Tar(f, bytes.NewReader(compress(t, f, tarBytes(t, files))))
		if err != nil {
			t.Fatal(f, err)
		}
		if got := readAll(t, tr); got["python/bin/python3.14"] != "ELF" {
			t.Fatalf("%s: got %v", f, got)
		}
		closeFn()
	}
}

func TestFormatOf(t *testing.T) {
	for name, want := range map[string]string{
		"libc6-dbg_2.43-2ubuntu2.7_amd64.deb":                                   Deb,
		"coreutils-dbgsym_9.4-3ubuntu6.3_amd64.ddeb":                            Deb,
		"cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-install_only.tar.gz":  TarGz,
		"cpython-3.14.7+20260825-x86_64-unknown-linux-gnu-pgo+lto-full.tar.zst": TarZst,
	} {
		if got, ok := FormatOf(name); !ok || got != want {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if _, ok := FormatOf("x.sha256"); ok {
		t.Error("a checksum file is not an archive")
	}
}

// TestRealDeb reads a real package when one is provided, because the synthetic
// archives above are written by the same assumptions they test.
func TestRealDeb(t *testing.T) {
	path := os.Getenv("PKGARCHIVE_TEST_DEB")
	if path == "" {
		t.Skip("set PKGARCHIVE_TEST_DEB to a .deb")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr, closeFn, err := Tar(Deb, f)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no files")
	}
	t.Logf("%d files", n)
}
