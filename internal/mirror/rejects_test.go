package mirror

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestRejectMemoryKeyedByAnnouncedHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rejects.json")
	m := NewRejectMemory(path)

	c := Candidate{BookID: "9780345463333", SourceBranchID: "src-1", ContentSHA256: "aaa"}
	if m.Skip(c) {
		t.Fatal("fresh candidate skipped")
	}
	m.Record(c)
	if !m.Skip(c) {
		t.Fatal("rejected candidate not skipped")
	}
	// A corrected announcement (new hash) makes the same book eligible again.
	healed := c
	healed.ContentSHA256 = "bbb"
	if m.Skip(healed) {
		t.Fatal("re-announced candidate wrongly skipped")
	}
	// Same book from a different source is unaffected.
	other := c
	other.SourceBranchID = "src-2"
	if m.Skip(other) {
		t.Fatal("other source wrongly skipped")
	}
	// Survives restart via persistence.
	if !NewRejectMemory(path).Skip(c) {
		t.Fatal("reject memory not persisted")
	}
}

func TestRecordRejectOnlyRemembersPermanent(t *testing.T) {
	m := &Manager{
		events:         newEventBuffer(5),
		rejects:        NewRejectMemory(""),
		reportCounters: make(map[string]*sourceReport),
	}
	transient := Candidate{BookID: "b1", SourceBranchID: "s1", ContentSHA256: "h1"}
	m.recordReject(transient, fmt.Errorf("download b1: connection reset"))
	if m.rejects.Skip(transient) {
		t.Fatal("transient failure permanently recorded")
	}
	permanent := Candidate{BookID: "b2", SourceBranchID: "s1", ContentSHA256: "h2"}
	m.recordReject(permanent, permrejectf("reject %s: hash mismatch (got=x announced=y)", permanent.BookID))
	if !m.rejects.Skip(permanent) {
		t.Fatal("permanent failure not recorded")
	}
	if m.rejects.Len() != 1 {
		t.Fatal("unexpected reject count", m.rejects.Len())
	}
}
