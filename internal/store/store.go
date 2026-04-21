package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
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

// Store is a single-node key-value store backed by a binary append-only log.
// The in-memory index maps each key to the byte offset of its latest log entry.
// All operations acquire a mutex to prevent interleaved log entries and index corruption.
type Store struct {
	mu       sync.Mutex
	file     *os.File
	index    map[string]int64 // key → byte offset of latest record
	writePos int64            // current end-of-file; next write goes here
}

// Open opens (or creates) the log file at path and replays it to rebuild the index.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	s := &Store{
		file:  f,
		index: make(map[string]int64),
	}
	if err := s.replay(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// replay scans the log from byte 0, rebuilds the index, and sets writePos.
// A torn tail write (io.ErrUnexpectedEOF or last-record CRC mismatch) is
// recovered by truncating the file to the last good record.
// A CRC mismatch on a non-final record returns ErrCorrupt.
func (s *Store) replay() error {
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	fileSize := info.Size()
	var offset int64

	for offset < fileSize {
		op, key, _, size, err := ReadRecordAt(s.file, offset)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// Partial record at tail: truncate and recover.
				return s.truncateToOffset(offset)
			}
			if errors.Is(err, ErrCorrupt) {
				// Full record present but CRC wrong.
				if offset+size >= fileSize {
					// Last record: treat as tail corruption (partial OS flush).
					return s.truncateToOffset(offset)
				}
				// Mid-file: serious corruption, do not silently skip.
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
	return s.file.Sync()
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
	return s.file.Sync()
}
