package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// makeELF builds the smallest file BuildIDFromELF has to understand: a header,
// one PT_NOTE program header, and the note itself. gap pads between the program
// header table and the note so the tests can also exercise the forward skip.
func makeELF(class byte, order binary.ByteOrder, buildID []byte, gap int) []byte {
	note := makeNote("GNU", ntGNUBuildID, buildID)

	hdrSize, phentsize := elf64HeaderSize, 56
	if class == 1 {
		hdrSize, phentsize = elf32HeaderSize, 32
	}
	phoff := hdrSize
	noteOff := phoff + phentsize + gap

	buf := make([]byte, noteOff+len(note))
	copy(buf, "\x7fELF")
	buf[4], buf[5], buf[6] = class, byteOrderID(order), 1

	ph := buf[phoff:]
	if class == 1 {
		order.PutUint32(buf[28:], uint32(phoff))
		order.PutUint16(buf[42:], uint16(phentsize))
		order.PutUint16(buf[44:], 1)
		order.PutUint32(ph[0:], ptNote)
		order.PutUint32(ph[4:], uint32(noteOff))
		order.PutUint32(ph[16:], uint32(len(note)))
	} else {
		order.PutUint64(buf[32:], uint64(phoff))
		order.PutUint16(buf[54:], uint16(phentsize))
		order.PutUint16(buf[56:], 1)
		order.PutUint32(ph[0:], ptNote)
		order.PutUint64(ph[8:], uint64(noteOff))
		order.PutUint64(ph[32:], uint64(len(note)))
	}
	copy(buf[noteOff:], note)
	return buf
}

func byteOrderID(order binary.ByteOrder) byte {
	if order == binary.BigEndian {
		return 2
	}
	return 1
}

func makeNote(name string, typ uint32, desc []byte) []byte {
	var b bytes.Buffer
	order := binary.LittleEndian
	binary.Write(&b, order, uint32(len(name)+1))
	binary.Write(&b, order, uint32(len(desc)))
	binary.Write(&b, order, typ)
	b.WriteString(name)
	b.WriteByte(0)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	b.Write(desc)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// makeNoteBE is makeNote with the header words in big-endian order, needed by
// the s390x/mips side of the archive.
func makeNoteBE(name string, typ uint32, desc []byte) []byte {
	n := makeNote(name, typ, desc)
	for i := 0; i < 12; i += 4 {
		n[i], n[i+1], n[i+2], n[i+3] = n[i+3], n[i+2], n[i+1], n[i]
	}
	return n
}

const testID = "544a5489c8d113e3639ccbd8d417af24dcbb3722"

func testIDBytes(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(testID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// countingReader reports how much of a stream was actually consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestBuildIDFromELFAcrossClassesAndOrders covers all four shapes Debian ships:
// amd64/arm64 are 64-bit LE, i386/armhf are 32-bit LE, s390x is 64-bit BE and
// mips is 32-bit BE. The 32-bit cases are the ones that used to fail, because
// the parser read a fixed 64-byte header and so overshot e_phoff at 28.
func TestBuildIDFromELFAcrossClassesAndOrders(t *testing.T) {
	want := testIDBytes(t)
	for _, tc := range []struct {
		name  string
		class byte
		order binary.ByteOrder
	}{
		{"elf64le", 2, binary.LittleEndian},
		{"elf32le", 1, binary.LittleEndian},
		{"elf64be", 2, binary.BigEndian},
		{"elf32be", 1, binary.BigEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := makeELF(tc.class, tc.order, want, 0)
			if tc.order == binary.BigEndian {
				buf = rewriteNoteBE(buf, want)
			}
			got, err := BuildIDFromELF(bytes.NewReader(buf), int64(len(buf)))
			if err != nil {
				t.Fatalf("BuildIDFromELF: %v", err)
			}
			if got != testID {
				t.Errorf("build id = %q, want %q", got, testID)
			}
		})
	}
}

// rewriteNoteBE swaps the note header of an already-built image to big endian.
func rewriteNoteBE(buf []byte, id []byte) []byte {
	le := makeNote("GNU", ntGNUBuildID, id)
	be := makeNoteBE("GNU", ntGNUBuildID, id)
	i := bytes.Index(buf, le)
	if i < 0 {
		panic("note not found")
	}
	copy(buf[i:], be)
	return buf
}

// The note need not sit right behind the program headers; the parser has to skip
// forward to it without ever seeking back.
func TestBuildIDFromELFSkipsForwardToNote(t *testing.T) {
	buf := makeELF(2, binary.LittleEndian, testIDBytes(t), 4096)
	got, err := BuildIDFromELF(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		t.Fatalf("BuildIDFromELF: %v", err)
	}
	if got != testID {
		t.Errorf("build id = %q, want %q", got, testID)
	}
}

// Only the GNU/NT_GNU_BUILD_ID note is a build id. A note segment full of
// something else - ABI tags, property notes, a vendor's own - must not be
// mistaken for one, or the index fills with keys nothing can ever match.
func TestBuildIDFromELFIgnoresOtherNotes(t *testing.T) {
	id := testIDBytes(t)
	notes := append(makeNote("GNU", 1 /* NT_GNU_ABI_TAG */, []byte("\x00\x00\x00\x00")), // wrong type
		append(makeNote("Xen", ntGNUBuildID, []byte("nope")), // wrong name
			makeNote("GNU", ntGNUBuildID, id)...)...)

	buf := makeELF(2, binary.LittleEndian, id, 0)
	buf = replaceTail(buf, makeNote("GNU", ntGNUBuildID, id), notes)

	got, err := BuildIDFromELF(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		t.Fatalf("BuildIDFromELF: %v", err)
	}
	if got != testID {
		t.Errorf("build id = %q, want %q", got, testID)
	}
}

func replaceTail(buf, old, new []byte) []byte {
	i := bytes.Index(buf, old)
	if i < 0 {
		panic("segment not found")
	}
	out := append(append([]byte{}, buf[:i]...), new...)
	// The note segment's p_filesz has to grow with it.
	binary.LittleEndian.PutUint64(out[elf64HeaderSize+32:], uint64(len(new)))
	return out
}

// A .deb's /usr/lib/debug is not all ELF - READMEs, .dwz indexes and gdb python
// scripts live there too. Those must come back as a skip, not an error, or every
// package containing one is filed as failed.
func TestBuildIDFromELFRejectsNonELF(t *testing.T) {
	for name, body := range map[string]string{
		"text":       "this is a README, not an ELF\n",
		"empty":      "",
		"short":      "\x7fELF",
		"bad magic":  strings.Repeat("\x00", 128),
		"bad class":  "\x7fELF\x09\x01\x01" + strings.Repeat("\x00", 121),
		"bad endian": "\x7fELF\x02\x09\x01" + strings.Repeat("\x00", 121),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildIDFromELF(strings.NewReader(body), int64(len(body)))
			if !errors.Is(err, errNotELF) {
				t.Errorf("err = %v, want errNotELF", err)
			}
		})
	}
}

// A relocatable object has no program headers, so it can carry no note segment -
// python-build-standalone's asm_trampoline.o is the canonical example. That is a
// missing build id, not a broken file.
func TestBuildIDFromELFRelocatableHasNoNote(t *testing.T) {
	buf := makeELF(2, binary.LittleEndian, testIDBytes(t), 0)
	binary.LittleEndian.PutUint16(buf[56:], 0) // e_phnum = 0

	_, err := BuildIDFromELF(bytes.NewReader(buf), int64(len(buf)))
	if !errors.Is(err, ErrNoBuildID) {
		t.Errorf("err = %v, want ErrNoBuildID", err)
	}
}

// A corrupt e_phoff must not make us read. With the length known - which it is
// for every tar entry - an offset past the end is rejected outright.
func TestBuildIDFromELFRejectsOffsetPastEnd(t *testing.T) {
	buf := makeELF(2, binary.LittleEndian, testIDBytes(t), 0)
	binary.LittleEndian.PutUint64(buf[32:], 1<<40) // e_phoff far past EOF

	r := &countingReader{r: io.MultiReader(bytes.NewReader(buf), endlessZeros{})}
	if _, err := BuildIDFromELF(r, int64(len(buf))); err == nil {
		t.Fatal("expected an error")
	}
	if r.n > elf64HeaderSize {
		t.Errorf("read %d bytes past the header, want none", r.n-elf64HeaderSize)
	}
}

// With the length unknown the fallback cap is what stops an endless read.
func TestBuildIDFromELFBoundsUnknownLengthSearch(t *testing.T) {
	buf := makeELF(2, binary.LittleEndian, testIDBytes(t), 0)
	binary.LittleEndian.PutUint64(buf[32:], 1<<40)

	r := &countingReader{r: io.MultiReader(bytes.NewReader(buf), endlessZeros{})}
	if _, err := BuildIDFromELF(r, 0); err == nil {
		t.Fatal("expected an error")
	}
	if r.n > maxNoteSearch+elf64HeaderSize {
		t.Errorf("read %d bytes, want at most ~%d", r.n, maxNoteSearch)
	}
}

// The note is NOT near the front. python-build-standalone links its
// interpreters so that PT_NOTE lands at the very end - python3.10 is 46 MB with
// its note segment at 0x2c3d0a8, 66 kB before EOF. A parser that gives up after
// a fixed prefix finds nothing there, and does so silently: the build id most
// worth having, the interpreter's own, is the one it drops.
func TestBuildIDFromELFFindsNoteAtEndOfFile(t *testing.T) {
	const distance = 40 << 20
	buf := makeELF(2, binary.LittleEndian, testIDBytes(t), distance)

	got, err := BuildIDFromELF(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		t.Fatalf("BuildIDFromELF: %v", err)
	}
	if got != testID {
		t.Errorf("build id = %q, want %q", got, testID)
	}
}

type endlessZeros struct{}

func (endlessZeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
