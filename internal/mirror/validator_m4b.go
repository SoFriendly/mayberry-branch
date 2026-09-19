package mirror

import (
	"encoding/binary"
	"fmt"
	"os"
)

// M4B files are ISO Base Media Format (MP4) containers: a flat sequence
// of length-prefixed "boxes" (atoms). We don't decode audio — we only
// verify the container is structurally sound before promoting bytes a
// source handed us into the user's library:
//
//   - it opens with a valid ftyp box carrying an audiobook brand,
//   - every box's declared size stays within the file and the boxes tile
//     the file exactly end-to-end (no gaps, no overrun, no trailing junk),
//   - the required moov (metadata) and mdat (media) boxes are present.
//
// This rejects truncated downloads, files renamed to .m4b, and random
// data whose first 12 bytes happen to look like an ftyp header, without
// the cost or attack surface of a full demux. Structure is walked by
// seeking box-to-box, so a 2 GB audiobook costs a few dozen small reads.

// maxM4BBoxes bounds the walk so a malformed file advertising millions of
// tiny boxes can't spin us. A real audiobook has well under a thousand
// top-level boxes.
const maxM4BBoxes = 100000

func validateM4B(path string, maxBytes int64) error {
	f, err := os.Open(path)
	if err != nil {
		return &ValidationError{Reason: "m4b open: " + err.Error()}
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return &ValidationError{Reason: "m4b stat: " + err.Error()}
	}
	size := info.Size()
	if maxBytes > 0 && size > maxBytes {
		return &ValidationError{Reason: fmt.Sprintf("m4b too large: %d > %d", size, maxBytes)}
	}
	if size < 16 {
		return &ValidationError{Reason: "m4b too small to contain ftyp"}
	}

	var (
		offset   int64
		sawFtyp  bool
		sawMoov  bool
		sawMdat  bool
		header   [16]byte
		firstBox = true
	)
	for i := 0; offset < size; i++ {
		if i >= maxM4BBoxes {
			return &ValidationError{Reason: "m4b has too many boxes"}
		}
		// A box header is 8 bytes (size32 + type); a 64-bit size adds 8.
		if size-offset < 8 {
			return &ValidationError{Reason: "m4b truncated box header"}
		}
		if _, err := f.ReadAt(header[:8], offset); err != nil {
			return &ValidationError{Reason: "m4b read box header: " + err.Error()}
		}
		boxSize := int64(binary.BigEndian.Uint32(header[0:4]))
		boxType := string(header[4:8])
		headerLen := int64(8)

		switch boxSize {
		case 1:
			// 64-bit largesize in the 8 bytes following the type.
			if size-offset < 16 {
				return &ValidationError{Reason: "m4b truncated 64-bit box size"}
			}
			if _, err := f.ReadAt(header[8:16], offset+8); err != nil {
				return &ValidationError{Reason: "m4b read 64-bit size: " + err.Error()}
			}
			boxSize = int64(binary.BigEndian.Uint64(header[8:16]))
			headerLen = 16
		case 0:
			// Size 0 means "to end of file" — only legal for the last box.
			boxSize = size - offset
		}
		if boxSize < headerLen {
			return &ValidationError{Reason: fmt.Sprintf("m4b box %q size %d smaller than header", boxType, boxSize)}
		}
		if offset+boxSize > size {
			return &ValidationError{Reason: fmt.Sprintf("m4b box %q overruns file (%d > %d)", boxType, offset+boxSize, size)}
		}

		if firstBox {
			if boxType != "ftyp" {
				return &ValidationError{Reason: "m4b does not open with ftyp"}
			}
			// Re-check the brand from the box body (SniffKind saw only the
			// file head; here we read it from the box we're validating).
			var brand [4]byte
			if _, err := f.ReadAt(brand[:], offset+8); err != nil {
				return &ValidationError{Reason: "m4b read brand: " + err.Error()}
			}
			if !m4bBrands[string(brand[:])] {
				return &ValidationError{Reason: "m4b ftyp brand not an audiobook: " + string(brand[:])}
			}
			sawFtyp = true
			firstBox = false
		}
		switch boxType {
		case "moov":
			sawMoov = true
		case "mdat":
			sawMdat = true
		}
		offset += boxSize
	}

	if offset != size {
		return &ValidationError{Reason: "m4b boxes do not tile the file exactly"}
	}
	if !sawFtyp {
		return &ValidationError{Reason: "m4b missing ftyp"}
	}
	if !sawMoov {
		return &ValidationError{Reason: "m4b missing moov"}
	}
	if !sawMdat {
		return &ValidationError{Reason: "m4b missing mdat"}
	}
	return nil
}
