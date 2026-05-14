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

// SniffKind reads enough of path to identify the file format by magic
// bytes alone. We do NOT trust any advertised extension or MIME type
// from the source — bytes are the only authority.
//
// Today this recognizes EPUB. M4B detection (`ftyp` atom at offset 4
// with major brand in {M4A, M4B, mp42, isom}) lands when the M4B
// validator does; for now M4B-shaped files sniff as Unknown and get
// rejected.
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
		return &ValidationError{Reason: "m4b validation not yet enabled"}
	}
	return &ValidationError{Reason: "unknown file kind"}
}
