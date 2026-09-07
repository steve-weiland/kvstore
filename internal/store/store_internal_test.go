package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// A valid record at a wrong index offset must be REFUSED, not returned.
// ReadRecordAt's CRC proves "this is a well-formed record" — it cannot prove
// "this is the record for the key you asked about". If the index is ever
// wrong (the reachable path: a stale hint surviving a crash-misordered
// compaction rename — see fsyncDir), a valid record for a DIFFERENT key at
// that offset would be served as this key's value: no error, silently wrong
// data. The key check turns that into ErrCorrupt.
func TestGetRefusesRecordForDifferentKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "kv.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put("alpha", []byte("alpha-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("beta", []byte("beta-value")); err != nil {
		t.Fatal(err)
	}

	// Corrupt the index the way a stale hint would: alpha's entry points at
	// beta's (perfectly valid, CRC-passing) record.
	s.mu.Lock()
	s.index["alpha"] = s.index["beta"]
	s.mu.Unlock()

	v, err := s.Get("alpha")
	if err == nil {
		t.Fatalf("Get(alpha) returned %q — another key's value, silently", v)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}
