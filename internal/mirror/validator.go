package mirror

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// Kind enumerates the file formats the mirror manager knows how to
// validate and store. Anything sniffed as KindUnknown is rejected; we
// never write bytes we can't reason about.
type Kind int

const (
	KindUnknown Kind = iota
	KindEPUB
	KindM4B
)

func (k Kind) String() string {
	switch k {
	case KindEPUB:
		return "epub"
	case KindM4B:
		return "m4b"
	}
	return "unknown"
}

// Ext returns the file extension Kind should be stored with (including
// the leading dot). Callers must use this rather than trusting any
// source-supplied extension.
func (k Kind) Ext() string {
	switch k {
	case KindEPUB:
		return ".epub"
	case KindM4B:
		return ".m4b"
	}
	return ""
}

// zipMagic is the local file header signature for a zip file (and
// therefore an EPUB, which is just a zip with specific contents).
var zipMagic = []byte{0x50, 0x4B, 0x03, 0x04}

// m4bBrands are the ISO-BMFF major brands (bytes 8:12 of an mp4 file's
// ftyp box) we accept as an audiobook. Audible/most encoders stamp M4A or
// M4B; many general encoders use isom/mp42/mp41/iso2, so we accept those
// too and rely on validateM4B to confirm the container is well-formed.
var m4bBrands = map[string]bool{
	"M4A ": true, "M4B ": true, "mp42": true,
	"mp41": true, "isom": true, "iso2": true,
}

// ftypMagic is the box type at offset 4 of an ISO base media file.
var ftypMagic = []byte("ftyp")

// SniffKind reads enough of path to identify the file format by magic
// bytes alone. We do NOT trust any advertised extension or MIME type
// from the source — bytes are the only authority.
//
// Recognizes EPUB (zip magic) and M4B (an ISO-BMFF `ftyp` box at offset 4
// carrying an audio major brand). Everything else sniffs as Unknown and
// is rejected; validateM4B then confirms the container structure.
func SniffKind(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return KindUnknown, fmt.Errorf("mirror: sniff open: %w", err)
	}
	defer f.Close()

	var head [16]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return KindUnknown, fmt.Errorf("mirror: sniff read: %w", err)
	}
	if n < 4 {
		return KindUnknown, nil
	}
	if bytes.Equal(head[0:4], zipMagic) {
		return KindEPUB, nil
	}
	if n >= 12 && bytes.Equal(head[4:8], ftypMagic) && m4bBrands[string(head[8:12])] {
		return KindM4B, nil
	}
	return KindUnknown, nil
}

// ValidationError signals that bytes downloaded into staging are not
// safe to promote. The reason is suitable for the audit log; do not
// expose it to remote callers (sources can use rejection reasons to
// fingerprint our validator).
type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string {
	return "mirror validation: " + e.Reason
}

// Validate dispatches to the right per-format validator. maxBytes is the
// per-file cap the caller already enforced during download; the
// validator may use it for additional sanity checks.
func Validate(path string, kind Kind, maxBytes int64) error {
	switch kind {
	case KindEPUB:
		return validateEPUB(path, maxBytes)
	case KindM4B:
		return validateM4B(path, maxBytes)
	}
	return &ValidationError{Reason: "unknown file kind"}
}
