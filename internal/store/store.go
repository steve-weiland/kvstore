package store

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
)

const maxLineSize = 128 * 1024 // 1 KB key + 64 KB value base64-encoded ≈ 88 KB; 128 KB is safe headroom

var ErrNotFound = errors.New("key not found")

type entry struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"` // base64-encoded; omitted on tombstones
}

// Store is a single-node key-value store backed by an append-only log.
// The in-memory index maps each key to the byte offset of its latest log entry.
// All operations acquire a mutex to prevent interleaved log entries and index corruption.
type Store struct {
	mu       sync.Mutex
	file     *os.File
	index    map[string]int64 // key → byte offset of latest entry line
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

// replay scans the log from byte 0, rebuilds the index, and sets writePos to the end of the log.
func (s *Store) replay() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, maxLineSize), maxLineSize)

	var offset int64
	for scanner.Scan() {
		line := scanner.Bytes()
		lineLen := int64(len(line)) + 1 // Scanner strips '\n'; account for it

		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			// V1: skip corrupt lines silently (no checksums yet)
			offset += lineLen
			continue
		}

		switch e.Op {
		case "put":
			s.index[e.Key] = offset
		case "del":
			delete(s.index, e.Key)
		}
		offset += lineLen
	}
	s.writePos = offset
	return scanner.Err()
}

// Get returns the value for key, or ErrNotFound if the key is absent or tombstoned.
func (s *Store) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	offset, ok := s.index[key]
	if !ok {
		return nil, ErrNotFound
	}

	if _, err := s.file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	// Fresh reader each call: reusing a buffered reader across seeks would return stale buffered data.
	line, err := bufio.NewReader(s.file).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(e.Value)
}

// Put appends a PUT entry to the log and updates the index.
func (s *Store) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := entry{Op: "put", Key: key, Value: base64.StdEncoding.EncodeToString(value)}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	offset := s.writePos
	if _, err := s.file.WriteAt(line, offset); err != nil {
		return err
	}
	s.writePos += int64(len(line))
	s.index[key] = offset
	return nil
}

// Delete appends a tombstone entry to the log and removes the key from the index.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.index[key]; !ok {
		return ErrNotFound
	}

	e := entry{Op: "del", Key: key}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if _, err := s.file.WriteAt(line, s.writePos); err != nil {
		return err
	}
	s.writePos += int64(len(line))
	delete(s.index, key)
	return nil
}
