package mirror

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// box builds an ISO-BMFF box: 4-byte big-endian size, 4-byte type, body.
func box(typ string, body []byte) []byte {
	b := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(body)))
	copy(b[4:8], typ)
	copy(b[8:], body)
	return b
}

func ftyp(brand string) []byte { return box("ftyp", []byte(brand+"\x00\x00\x00\x00")) }

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.m4b")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSniffKindM4B(t *testing.T) {
	valid := append(ftyp("M4B "), box("moov", []byte("x"))...)
	if k, _ := SniffKind(writeTemp(t, valid)); k != KindM4B {
		t.Fatal("valid M4B not sniffed")
	}
	// ftyp present but a non-audio brand is not ours.
	if k, _ := SniffKind(writeTemp(t, ftyp("qt  "))); k != KindUnknown {
		t.Fatal("non-audiobook brand sniffed as M4B")
	}
	// Random bytes.
	if k, _ := SniffKind(writeTemp(t, []byte("not a media file at all"))); k != KindUnknown {
		t.Fatal("garbage sniffed as a known kind")
	}
	// EPUB (zip magic) still wins.
	if k, _ := SniffKind(writeTemp(t, []byte{0x50, 0x4B, 0x03, 0x04, 0, 0, 0, 0})); k != KindEPUB {
		t.Fatal("zip not sniffed as EPUB")
	}
}

func TestValidateM4B(t *testing.T) {
	good := func() []byte {
		return concat(ftyp("M4B "), box("moov", make([]byte, 40)), box("mdat", make([]byte, 128)))
	}
	if err := validateM4B(writeTemp(t, good()), MaxAudiobookBytes); err != nil {
		t.Fatalf("valid M4B rejected: %v", err)
	}

	cases := map[string][]byte{
		"missing moov":       concat(ftyp("M4B "), box("mdat", make([]byte, 64))),
		"missing mdat":       concat(ftyp("M4B "), box("moov", make([]byte, 64))),
		"no ftyp first":      concat(box("moov", make([]byte, 16)), box("mdat", make([]byte, 16))),
		"bad brand":          concat(ftyp("qt  "), box("moov", nil), box("mdat", nil)),
		"trailing junk":      append(good(), 0x00, 0x01, 0x02),
		"truncated last box": good()[:len(good())-10],
	}
	for name, data := range cases {
		if err := validateM4B(writeTemp(t, data), MaxAudiobookBytes); err == nil {
			t.Fatalf("%s: expected rejection, got nil", name)
		}
	}

	// A box claiming a huge size must be caught as overrun, not read into.
	overrun := ftyp("M4B ")
	binary.BigEndian.PutUint32(overrun[0:4], 1<<30)
	if err := validateM4B(writeTemp(t, overrun), MaxAudiobookBytes); err == nil {
		t.Fatal("overrunning box accepted")
	}

	// A 64-bit-size box (size32==1) that is well-formed validates.
	big := make([]byte, 16)
	copy(big[4:8], "mdat")
	binary.BigEndian.PutUint32(big[0:4], 1)
	binary.BigEndian.PutUint64(big[8:16], 16) // whole box is just the header
	valid64 := concat(ftyp("M4B "), box("moov", nil), big)
	if err := validateM4B(writeTemp(t, valid64), MaxAudiobookBytes); err != nil {
		t.Fatalf("valid 64-bit-size box rejected: %v", err)
	}

	// Size cap is enforced.
	if err := validateM4B(writeTemp(t, good()), 8); err == nil {
		t.Fatal("oversize file accepted under tiny cap")
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
