// Package audiobook extracts metadata from M4B (MP4 audio) audiobook files.
//
// We parse just enough of the MP4 atom structure to read:
//   - iTunes-style metadata (©nam, ©ART, ©wrt, ©alb, ©day, ©cmt, covr) from moov/udta/meta/ilst
//   - Total duration from moov/mvhd
//
// We don't depend on any third-party MP4 library — the file format is simple
// enough that walking the box tree by hand is straightforward.
package audiobook

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxCoverSize = 5 << 20 // 5 MB cap, same as EPUB.

// Metadata holds audiobook metadata extracted from an M4B file.
type Metadata struct {
	Title           string
	Author          string
	Narrator        string
	Album           string
	Year            string
	Description     string
	Genres          []string
	DurationSeconds int
	CoverData       []byte
	CoverType       string // "image/jpeg" or "image/png"
	ASIN            string // Amazon Standard ID for Audnexus lookup, if present
}

// asinRe matches a 10-character Audible ASIN: starts with B0, then 8
// alphanumerics. Audible's own m4b downloads have the ASIN in the filename;
// many community-tagged files embed it in iTunes metadata.
var asinRe = regexp.MustCompile(`B0[0-9A-Z]{8}`)

// findASINInFilename returns the first ASIN-looking substring in the path's
// base name, or empty string.
func findASINInFilename(path string) string {
	base := strings.ToUpper(filepath.Base(path))
	if m := asinRe.FindString(base); m != "" {
		return m
	}
	return ""
}

// ExtractMetadata reads the M4B file at path and returns its metadata.
func ExtractMetadata(path string) (Metadata, error) {
	var m Metadata
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return m, err
	}

	if err := walkAtoms(f, 0, info.Size(), &m, nil); err != nil {
		return m, err
	}
	// Title may be empty — many m4b files don't have iTunes metadata. The
	// caller is expected to fall back to the filename in that case.
	// ASIN: prefer embedded freeform atom (already populated by walkAtoms),
	// fall back to filename pattern matching for Audible-named files.
	if m.ASIN == "" {
		m.ASIN = findASINInFilename(path)
	}
	return m, nil
}

// walkAtoms recursively descends through MP4 box structure, populating Metadata.
// path is the slash-joined atom names from the root (for matching nested boxes).
func walkAtoms(r io.ReaderAt, offset, end int64, m *Metadata, path []string) error {
	for offset < end {
		size, name, headerSize, err := readBoxHeader(r, offset)
		if err != nil {
			return err
		}
		if size == 0 || size < int64(headerSize) {
			return nil // bad/end-of-stream
		}
		bodyStart := offset + int64(headerSize)
		bodyEnd := offset + size
		fullPath := append(path, name)

		switch name {
		case "moov", "trak", "mdia", "minf", "stbl", "udta":
			if err := walkAtoms(r, bodyStart, bodyEnd, m, fullPath); err != nil {
				return err
			}
		case "meta":
			// Apple's `meta` atom has a 4-byte version/flags prefix before children.
			if err := walkAtoms(r, bodyStart+4, bodyEnd, m, fullPath); err != nil {
				return err
			}
		case "ilst":
			if err := walkIlst(r, bodyStart, bodyEnd, m); err != nil {
				return err
			}
		case "mvhd":
			parseMvhd(r, bodyStart, bodyEnd, m)
		}
		offset = bodyEnd
	}
	return nil
}

// readBoxHeader reads an MP4 box header at the given offset.
// Returns the box size (including header), 4-byte name, and header length.
func readBoxHeader(r io.ReaderAt, offset int64) (int64, string, int, error) {
	var head [8]byte
	if _, err := r.ReadAt(head[:], offset); err != nil {
		return 0, "", 0, err
	}
	size := int64(binary.BigEndian.Uint32(head[0:4]))
	name := string(head[4:8])
	headerSize := 8
	if size == 1 {
		// 64-bit largesize follows the standard header.
		var ext [8]byte
		if _, err := r.ReadAt(ext[:], offset+8); err != nil {
			return 0, "", 0, err
		}
		size = int64(binary.BigEndian.Uint64(ext[:]))
		headerSize = 16
	}
	return size, name, headerSize, nil
}

// walkIlst iterates the children of an `ilst` atom, where each child is a
// metadata field. Inside each field there's a `data` box containing the value.
func walkIlst(r io.ReaderAt, offset, end int64, m *Metadata) error {
	for offset < end {
		size, name, headerSize, err := readBoxHeader(r, offset)
		if err != nil {
			return err
		}
		if size == 0 || size < int64(headerSize) {
			return nil
		}
		fieldStart := offset + int64(headerSize)
		fieldEnd := offset + size

		// Inside this field, find the `data` box.
		dataOff := fieldStart
		for dataOff < fieldEnd {
			dsize, dname, dhead, err := readBoxHeader(r, dataOff)
			if err != nil || dsize == 0 || dsize < int64(dhead) {
				break
			}
			if dname == "data" {
				applyField(r, name, dataOff+int64(dhead), dataOff+dsize, m)
				break
			}
			dataOff += dsize
		}
		offset = fieldEnd
	}
	return nil
}

// applyField interprets a single iTunes metadata field and updates Metadata.
// The 4-byte type and 4-byte locale prefix the value bytes.
func applyField(r io.ReaderAt, name string, valueStart, valueEnd int64, m *Metadata) {
	if valueEnd-valueStart < 8 {
		return
	}
	var typeHeader [8]byte
	if _, err := r.ReadAt(typeHeader[:], valueStart); err != nil {
		return
	}
	typeCode := binary.BigEndian.Uint32(typeHeader[0:4])
	bodyStart := valueStart + 8
	bodyLen := valueEnd - bodyStart
	if bodyLen <= 0 {
		return
	}

	switch name {
	case "\xa9nam":
		m.Title = readText(r, bodyStart, bodyLen)
	case "\xa9ART", "aART":
		if m.Author == "" {
			m.Author = readText(r, bodyStart, bodyLen)
		}
	case "\xa9wrt":
		m.Narrator = readText(r, bodyStart, bodyLen)
	case "\xa9alb":
		m.Album = readText(r, bodyStart, bodyLen)
	case "\xa9day":
		m.Year = readText(r, bodyStart, bodyLen)
	case "\xa9cmt", "desc":
		m.Description = readText(r, bodyStart, bodyLen)
	case "\xa9gen", "gnre":
		genre := readText(r, bodyStart, bodyLen)
		if genre != "" {
			m.Genres = append(m.Genres, genre)
		}
	case "covr":
		if bodyLen <= maxCoverSize {
			buf := make([]byte, bodyLen)
			if _, err := r.ReadAt(buf, bodyStart); err == nil {
				m.CoverData = buf
				switch typeCode {
				case 13:
					m.CoverType = "image/jpeg"
				case 14:
					m.CoverType = "image/png"
				default:
					m.CoverType = "image/jpeg"
				}
			}
		}
	}
}

func readText(r io.ReaderAt, offset, length int64) string {
	if length <= 0 || length > 64*1024 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := r.ReadAt(buf, offset); err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

// parseMvhd reads the movie header to derive duration in seconds.
// mvhd v0: version(1) flags(3) created(4) modified(4) timescale(4) duration(4) ...
// mvhd v1: version(1) flags(3) created(8) modified(8) timescale(4) duration(8) ...
func parseMvhd(r io.ReaderAt, start, end int64, m *Metadata) {
	if end-start < 24 {
		return
	}
	var ver [1]byte
	if _, err := r.ReadAt(ver[:], start); err != nil {
		return
	}
	var timescale uint32
	var duration uint64
	if ver[0] == 0 {
		var buf [20]byte
		if _, err := r.ReadAt(buf[:], start+4); err != nil {
			return
		}
		timescale = binary.BigEndian.Uint32(buf[8:12])
		duration = uint64(binary.BigEndian.Uint32(buf[12:16]))
	} else {
		var buf [28]byte
		if _, err := r.ReadAt(buf[:], start+4); err != nil {
			return
		}
		timescale = binary.BigEndian.Uint32(buf[16:20])
		duration = binary.BigEndian.Uint64(buf[20:28])
	}
	if timescale > 0 {
		m.DurationSeconds = int(duration / uint64(timescale))
	}
}
