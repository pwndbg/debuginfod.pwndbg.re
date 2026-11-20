package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ErrNoBuildID is returned by BuildIDFromELF for a well-formed ELF that simply
// carries no NT_GNU_BUILD_ID note. It is not a parse failure: a .deb full of
// such files is indexed as empty, not as broken.
var ErrNoBuildID = errors.New("no build-id note")

// errNotELF marks a file that is not an ELF at all. /usr/lib/debug holds plenty
// of them (.dwz multifiles aside, there are READMEs, symlinks, directories), so
// callers skip these silently.
var errNotELF = errors.New("not an ELF file")

const (
	ptNote       = 4 // PT_NOTE
	ntGNUBuildID = 3 // NT_GNU_BUILD_ID

	eIdentSize      = 16
	elf32HeaderSize = 52
	elf64HeaderSize = 64

	// maxNoteSearch bounds the forward scan when the stream length is unknown.
	//
	// It is NOT true that notes sit near the front. python-build-standalone's
	// interpreters put PT_NOTE at the very end - python3.10 is 46 MB and its
	// note segment starts at 0x2c3d0a8, 66 kB before EOF - so a small cap
	// silently loses exactly the binary a user is most likely to be debugging.
	// Callers that know the length (a tar entry header states it) pass it
	// instead and get an exact bound.
	maxNoteSearch = 1 << 30

	// maxNoteSegment caps one PT_NOTE segment, which is read into memory. Real
	// ones are under a hundred bytes; this only stops a corrupt p_filesz from
	// turning into an allocation.
	maxNoteSegment = 1 << 20
)

// BuildIDFromELF reads the GNU build-id out of an ELF file presented as a
// forward-only stream.
//
// It deliberately does not use debug/elf: that needs an io.ReaderAt, and the
// section headers it indexes through live at the *end* of the file, so every
// member of a tarball would have to be buffered whole. The note is reachable
// from the program headers alone, which do sit near the front, so a single
// forward pass suffices - though the note itself may be anywhere.
//
// size is the length of the stream, which a tar entry header states; pass 0
// when it is unknown. Knowing it makes an out-of-range offset an immediate
// error rather than a long read, and reading to the end of an entry costs
// nothing extra: tar has to stream past it to reach the next header anyway.
func BuildIDFromELF(r io.Reader, size int64) (string, error) {
	pos := int64(0)
	read := func(n int64) ([]byte, error) {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		pos += n
		return buf, nil
	}
	// skipTo moves the stream forward to off. Seeking backwards is impossible
	// here, so a segment we have already passed is reported rather than read.
	skipTo := func(off int64) error {
		if off < pos {
			return fmt.Errorf("offset %d already passed (at %d)", off, pos)
		}
		if size > 0 && off > size {
			return fmt.Errorf("offset %d past end of %d-byte stream", off, size)
		}
		if size <= 0 && off-pos > maxNoteSearch {
			return fmt.Errorf("offset %d too far ahead (at %d)", off, pos)
		}
		n, err := io.CopyN(io.Discard, r, off-pos)
		pos += n
		return err
	}

	// e_ident first, alone: the rest of the header is a different length for
	// each class, and reading the 64-bit size unconditionally would overshoot a
	// 32-bit file's 52-byte header - putting the stream past e_phoff, which
	// sits at 28 - and every i386 and armhf package would index as empty.
	ident, err := read(eIdentSize)
	if err != nil {
		// Anything shorter than e_ident cannot be an ELF.
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return "", errNotELF
		}
		return "", err
	}
	if !bytes.HasPrefix(ident, []byte("\x7fELF")) {
		return "", errNotELF
	}

	var (
		class = ident[4] // 1 = ELFCLASS32, 2 = ELFCLASS64
		data  = ident[5] // 1 = little endian, 2 = big endian
		order binary.ByteOrder
	)
	switch data {
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return "", fmt.Errorf("%w: bad EI_DATA %d", errNotELF, data)
	}

	headerSize := int64(elf32HeaderSize)
	if class == 2 {
		headerSize = elf64HeaderSize
	} else if class != 1 {
		return "", fmt.Errorf("%w: bad EI_CLASS %d", errNotELF, class)
	}
	rest, err := read(headerSize - eIdentSize)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return "", errNotELF
		}
		return "", err
	}
	// Reassembled so the field offsets below are the ones in the ELF spec.
	hdr := append(ident, rest...)

	var phoff int64
	var phentsize, phnum int
	if class == 1 {
		phoff = int64(order.Uint32(hdr[28:32]))
		phentsize = int(order.Uint16(hdr[42:44]))
		phnum = int(order.Uint16(hdr[44:46]))
	} else {
		phoff = int64(order.Uint64(hdr[32:40]))
		phentsize = int(order.Uint16(hdr[54:56]))
		phnum = int(order.Uint16(hdr[56:58]))
	}

	// A relocatable object (.o) has no program headers at all, so it can never
	// carry a note segment - and a linker note would be meaningless there
	// anyway, since the build-id is computed when the output is linked.
	if phnum == 0 || phoff == 0 {
		return "", ErrNoBuildID
	}
	minEnt := 32
	if class == 2 {
		minEnt = 56
	}
	if phentsize < minEnt || phnum > 1<<16 {
		return "", fmt.Errorf("%w: implausible program header table %dx%d", errNotELF, phnum, phentsize)
	}

	if err := skipTo(phoff); err != nil {
		return "", err
	}
	phdrs, err := read(int64(phentsize) * int64(phnum))
	if err != nil {
		return "", err
	}

	// Collect note segments first and visit them in file order: the program
	// header table is ordered by virtual address, which need not match.
	type segment struct{ off, size int64 }
	var notes []segment
	for i := range phnum {
		e := phdrs[i*phentsize:]
		var typ uint32
		var off, segSize int64
		if class == 2 {
			typ = order.Uint32(e[0:4])
			off = int64(order.Uint64(e[8:16]))
			segSize = int64(order.Uint64(e[32:40]))
		} else {
			typ = order.Uint32(e[0:4])
			off = int64(order.Uint32(e[4:8]))
			segSize = int64(order.Uint32(e[16:20]))
		}
		if typ == ptNote && segSize > 0 && segSize < maxNoteSegment {
			notes = append(notes, segment{off, segSize})
		}
	}
	slices.SortFunc(notes, func(a, b segment) int { return int(a.off - b.off) })

	for _, seg := range notes {
		if err := skipTo(seg.off); err != nil {
			continue // already passed, or absurdly far away
		}
		buf, err := read(seg.size)
		if err != nil {
			// A truncated final segment is not worth failing over if an earlier
			// one already answered; there is none left to try, so report it.
			return "", err
		}
		if id, ok := buildIDFromNotes(buf, order); ok {
			return id, nil
		}
	}
	return "", ErrNoBuildID
}

// buildIDFromNotes walks the note entries in one PT_NOTE segment. Entries are
// namesz/descsz/type headers followed by the name and the descriptor, each
// padded to 4 bytes - including in ELF64, where the spec says 8 but every
// toolchain in practice emits 4.
func buildIDFromNotes(buf []byte, order binary.ByteOrder) (string, bool) {
	const hexDigits = "0123456789abcdef"
	for len(buf) >= 12 {
		namesz := int(order.Uint32(buf[0:4]))
		descsz := int(order.Uint32(buf[4:8]))
		typ := order.Uint32(buf[8:12])
		nameEnd := 12 + align4(namesz)
		descEnd := nameEnd + align4(descsz)
		if namesz < 0 || descsz < 0 || descEnd > len(buf) || descEnd < nameEnd {
			return "", false
		}
		name := buf[12 : 12+namesz]
		if typ == ntGNUBuildID && string(bytes.TrimRight(name, "\x00")) == "GNU" && descsz > 0 {
			desc := buf[nameEnd : nameEnd+descsz]
			out := make([]byte, 0, len(desc)*2)
			for _, b := range desc {
				out = append(out, hexDigits[b>>4], hexDigits[b&0xf])
			}
			return string(out), true
		}
		buf = buf[descEnd:]
	}
	return "", false
}

func align4(n int) int { return (n + 3) &^ 3 }
