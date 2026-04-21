// kvdump decodes and prints every record in a kvstore binary log file.
//
// Usage:
//
//	kvdump [-log data/kv.log]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"unicode/utf8"

	"github.com/steve-weiland/kvstore/internal/store"
)

func main() {
	path := flag.String("log", "data/kv.log", "path to kvstore log file")
	flag.Parse()

	f, err := os.Open(*path)
	if err != nil {
		log.Fatalf("open %s: %v", *path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Fatal(err)
	}
	fileSize := info.Size()

	type entry struct {
		offset int64
		op     byte
		key    string
		value  []byte
		size   int64
	}

	var records []entry
	// index maps key → position in records slice for the latest live record.
	index := make(map[string]int)

	var offset int64
	for offset < fileSize {
		op, key, value, size, err := store.ReadRecordAt(f, offset)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				fmt.Fprintf(os.Stderr, "offset=%-10d  WARN: truncated record (torn write)\n", offset)
			} else if errors.Is(err, store.ErrCorrupt) {
				fmt.Fprintf(os.Stderr, "offset=%-10d  WARN: corrupt record (CRC mismatch)\n", offset)
			} else {
				fmt.Fprintf(os.Stderr, "offset=%-10d  ERROR: %v\n", offset, err)
			}
			break
		}

		records = append(records, entry{offset, op, key, value, size})
		switch op {
		case store.OpPut:
			index[key] = len(records) - 1
		case store.OpDel:
			delete(index, key)
		}
		offset += size
	}

	// Print all records, marking stale ones.
	liveOffsets := make(map[int64]bool, len(index))
	for _, i := range index {
		liveOffsets[records[i].offset] = true
	}

	for _, r := range records {
		stale := !liveOffsets[r.offset]
		marker := "    "
		if stale {
			marker = "STALE"
		}

		switch r.op {
		case store.OpPut:
			fmt.Printf("offset=%-10d  %s  op=PUT  key=%-24s  %4d bytes  value=%s\n",
				r.offset, marker, fmt.Sprintf("%q", r.key), r.size, formatValue(r.value))
		case store.OpDel:
			fmt.Printf("offset=%-10d  %s  op=DEL  key=%-24s  %4d bytes\n",
				r.offset, marker, fmt.Sprintf("%q", r.key), r.size)
		}
	}

	// Summary.
	var liveBytes int64
	for _, i := range index {
		liveBytes += records[i].size
	}
	staleBytes := fileSize - liveBytes
	stalePct := 0
	if fileSize > 0 {
		stalePct = int(100 * staleBytes / fileSize)
	}

	fmt.Printf("\nlive keys: %d  |  records: %d  |  file: %d bytes  |  stale: %d bytes (%d%%)\n",
		len(index), len(records), fileSize, staleBytes, stalePct)
}

// formatValue returns a human-readable representation of a value byte slice.
func formatValue(v []byte) string {
	if len(v) == 0 {
		return "—"
	}
	const maxDisplay = 40
	display := v
	truncated := false
	if len(display) > maxDisplay {
		display = display[:maxDisplay]
		truncated = true
	}
	var s string
	if utf8.Valid(display) {
		s = fmt.Sprintf("%q", display)
	} else {
		s = fmt.Sprintf("%x", display)
	}
	if truncated {
		s += fmt.Sprintf("… (%d bytes)", len(v))
	}
	return s
}
