package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Record layout:
//
//	┌──────────┬─────────┬─────────┬─────┬─────────────┬─────────────┐
//	│  crc32   │ key_sz  │ val_sz  │ op  │    key      │    value    │
//	│  4 bytes │ 2 bytes │ 4 bytes │ 1 B │ key_sz bytes│ val_sz bytes│
//	└──────────┴─────────┴─────────┴─────┴─────────────┴─────────────┘
//
// crc32 covers bytes [4..end]: key_sz, val_sz, op, key, value. Big-endian.
const RecordHeaderSize = 11 // 4 + 2 + 4 + 1

const (
	OpPut = byte(0x01)
	OpDel = byte(0x02)
)

var (
	ErrNotFound = errors.New("key not found")
	ErrCorrupt  = errors.New("corrupt log entry")
)

// Hint file layout:
//
//	[compact_size : 8 bytes]           ← data file size at compaction time
//	[key_sz : 2][val_sz : 4][val_pos : 8][key : key_sz bytes]  ← one per live key
const (
	hintFileHeaderSize  = 8
	hintEntryHeaderSize = 14 // key_sz(2) + val_sz(4) + val_pos(8)
)

type hintEntry struct {
	key    string
	valSz  uint32
	valPos int64
}

// EncodeRecord encodes a log record into the binary wire format.
// Exported for use by the hint file writer and tests.
func EncodeRecord(op byte, key string, value []byte) []byte {
	keySz := len(key)
	valSz := len(value)
	buf := make([]byte, RecordHeaderSize+keySz+valSz)

	binary.BigEndian.PutUint16(buf[4:6], uint16(keySz))
	binary.BigEndian.PutUint32(buf[6:10], uint32(valSz))
	buf[10] = op
	copy(buf[RecordHeaderSize:], key)
	copy(buf[RecordHeaderSize+keySz:], value)

	checksum := crc32.ChecksumIEEE(buf[4:])
	binary.BigEndian.PutUint32(buf[0:4], checksum)
	return buf
}

// ReadRecordAt reads and validates the record at offset in r.
// Returns the record fields, total size in bytes, and any error.
// ErrCorrupt means a full record was present but CRC mismatched.
// io.ErrUnexpectedEOF means the record was truncated (torn write).
// size is always populated from the header when available, even on error,
// so the caller can determine whether the corrupt record was the last one.
func ReadRecordAt(r io.ReaderAt, offset int64) (op byte, key string, value []byte, size int64, err error) {
	hdr := make([]byte, RecordHeaderSize)
	n, readErr := r.ReadAt(hdr, offset)
	if n < RecordHeaderSize {
		err = io.ErrUnexpectedEOF
		return
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		err = readErr
		return
	}

	keySz := int(binary.BigEndian.Uint16(hdr[4:6]))
	valSz := int(binary.BigEndian.Uint32(hdr[6:10]))
	size = int64(RecordHeaderSize + keySz + valSz)

	payload := make([]byte, keySz+valSz)
	if len(payload) > 0 {
		n, readErr = r.ReadAt(payload, offset+RecordHeaderSize)
		if n < len(payload) {
			err = io.ErrUnexpectedEOF
			return
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			err = readErr
			return
		}
	}

	storedCRC := binary.BigEndian.Uint32(hdr[0:4])
	h := crc32.NewIEEE()
	h.Write(hdr[4:]) // key_sz + val_sz + op
	h.Write(payload) // key + value
	if h.Sum32() != storedCRC {
		err = ErrCorrupt
		return
	}

	op = hdr[10]
	key = string(payload[:keySz])
	value = payload[keySz:] // empty slice for tombstones (valSz == 0)
	return
}

const defaultCompactionThreshold = 32 * 1024 * 1024 // 32 MB

// Store is a single-node key-value store backed by a binary append-only log.
// The in-memory index maps each key to the byte offset of its latest log entry.
// All operations acquire a mutex to prevent interleaved log entries and index corruption.
type Store struct {
	mu                  sync.Mutex
	file                *os.File
	path                string
	index               map[string]int64 // key → byte offset of latest record
	writePos            int64            // current end-of-file; next write goes here
	compactionThreshold int64
}

// Open opens (or creates) the log file at path and rebuilds the index.
// If a hint file exists it is loaded first (O(live keys)); the log is then
// replayed only from the compacted boundary to EOF to pick up any writes
// that occurred after the last compaction. Falls back to full log replay
// when no valid hint file is found.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	s := &Store{
		file:                f,
		path:                path,
		index:               make(map[string]int64),
		compactionThreshold: defaultCompactionThreshold,
	}
	compactEnd, ok := s.loadHintFile()
	if ok {
		if err := s.replayFrom(compactEnd); err != nil {
			f.Close()
			return nil, err
		}
	} else {
		if err := s.replayFrom(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return s, nil
}

// replayFrom scans the log from startOffset to EOF, updating the index and writePos.
// A torn tail (io.ErrUnexpectedEOF or last-record CRC mismatch) is recovered by
// truncating to the last good record. A CRC mismatch on a non-final record is fatal.
func (s *Store) replayFrom(startOffset int64) error {
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	fileSize := info.Size()
	offset := startOffset

	for offset < fileSize {
		op, key, _, size, err := ReadRecordAt(s.file, offset)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return s.truncateToOffset(offset)
			}
			if errors.Is(err, ErrCorrupt) {
				if offset+size >= fileSize {
					return s.truncateToOffset(offset)
				}
				return fmt.Errorf("corrupt record at offset %d: %w", offset, ErrCorrupt)
			}
			return err
		}

		switch op {
		case OpPut:
			s.index[key] = offset
		case OpDel:
			delete(s.index, key)
		}
		offset += size
	}
	s.writePos = offset
	return nil
}

// loadHintFile loads the hint file alongside the data file, populating s.index.
// Returns (compactedSize, true) on success so Open can replay only the tail.
// Returns (0, false) on any failure; the caller must fall back to full replay.
func (s *Store) loadHintFile() (int64, bool) {
	f, err := os.Open(s.path + ".hint")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	// 8-byte file header: data file size at compaction time.
	var hdr [hintFileHeaderSize]byte
	if n, err := f.ReadAt(hdr[:], 0); n < hintFileHeaderSize || (err != nil && !errors.Is(err, io.EOF)) {
		return 0, false
	}
	compactSize := int64(binary.BigEndian.Uint64(hdr[:]))

	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	fileSize := info.Size()
	offset := int64(hintFileHeaderSize)
	newIndex := make(map[string]int64)

	for offset < fileSize {
		var entHdr [hintEntryHeaderSize]byte
		n, err := f.ReadAt(entHdr[:], offset)
		if n < hintEntryHeaderSize || (err != nil && !errors.Is(err, io.EOF)) {
			return 0, false
		}
		keySz := int(binary.BigEndian.Uint16(entHdr[0:2]))
		valPos := int64(binary.BigEndian.Uint64(entHdr[6:14]))

		key := make([]byte, keySz)
		n, err = f.ReadAt(key, offset+hintEntryHeaderSize)
		if n < keySz || (err != nil && !errors.Is(err, io.EOF)) {
			return 0, false
		}
		newIndex[string(key)] = valPos
		offset += int64(hintEntryHeaderSize + keySz)
	}

	s.index = newIndex
	return compactSize, true
}

func (s *Store) truncateToOffset(offset int64) error {
	if err := s.file.Truncate(offset); err != nil {
		return err
	}
	s.writePos = offset
	return nil
}

// Close releases the underlying file descriptor.
func (s *Store) Close() error {
	return s.file.Close()
}

// Get returns the value for key, or ErrNotFound if the key is absent or tombstoned.
func (s *Store) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	offset, ok := s.index[key]
	if !ok {
		return nil, ErrNotFound
	}

	_, _, value, _, err := ReadRecordAt(s.file, offset)
	if err != nil {
		return nil, err
	}
	return value, nil
}

// Put appends a PUT record to the log and updates the index.
func (s *Store) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := EncodeRecord(OpPut, key, value)
	offset := s.writePos
	if _, err := s.file.WriteAt(rec, offset); err != nil {
		return err
	}
	s.writePos += int64(len(rec))
	s.index[key] = offset
	if err := s.file.Sync(); err != nil {
		return err
	}
	if s.writePos >= s.compactionThreshold {
		_ = s.compact()
	}
	return nil
}

// Delete appends a tombstone record to the log and removes the key from the index.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.index[key]; !ok {
		return ErrNotFound
	}

	rec := EncodeRecord(OpDel, key, nil)
	if _, err := s.file.WriteAt(rec, s.writePos); err != nil {
		return err
	}
	s.writePos += int64(len(rec))
	delete(s.index, key)
	if err := s.file.Sync(); err != nil {
		return err
	}
	if s.writePos >= s.compactionThreshold {
		_ = s.compact()
	}
	return nil
}

// Compact rewrites the log to contain only live keys, atomically replacing the data file.
// It is safe to call concurrently — it acquires the store lock for the duration.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compact()
}

// compact is the lock-held implementation of Compact.
func (s *Store) compact() error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "kv-compact-*.log")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	hints := make([]hintEntry, 0, len(s.index))
	newIndex := make(map[string]int64, len(s.index))
	var pos int64
	for key, offset := range s.index {
		_, _, value, _, err := ReadRecordAt(s.file, offset)
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("compact: read %q: %w", key, err)
		}
		rec := EncodeRecord(OpPut, key, value)
		if _, err := tmp.WriteAt(rec, pos); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return err
		}
		newIndex[key] = pos
		hints = append(hints, hintEntry{key, uint32(len(value)), pos})
		pos += int64(len(rec))
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Remove stale hint before renaming data file: if we crash between the data
	// rename and the new hint write, Open will fall back to full replay rather
	// than loading a hint that points into a now-different data file.
	os.Remove(s.path + ".hint")

	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Best-effort: a missing hint file triggers full replay on the next Open.
	_ = s.writeHintFile(pos, hints)

	s.file.Close()
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	s.file = f
	s.index = newIndex
	s.writePos = pos
	return nil
}

// writeHintFile atomically writes the hint file alongside the data file.
// compactSize is the total byte length of the just-compacted data file.
func (s *Store) writeHintFile(compactSize int64, hints []hintEntry) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "kv-hint-*.hint")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	// 8-byte header: data file size at compaction time.
	var hdr [hintFileHeaderSize]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(compactSize))
	if _, err := tmp.WriteAt(hdr[:], 0); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}

	off := int64(hintFileHeaderSize)
	for _, h := range hints {
		buf := make([]byte, hintEntryHeaderSize+len(h.key))
		binary.BigEndian.PutUint16(buf[0:2], uint16(len(h.key)))
		binary.BigEndian.PutUint32(buf[2:6], h.valSz)
		binary.BigEndian.PutUint64(buf[6:14], uint64(h.valPos))
		copy(buf[hintEntryHeaderSize:], h.key)
		if _, err := tmp.WriteAt(buf, off); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return err
		}
		off += int64(len(buf))
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, s.path+".hint")
}
