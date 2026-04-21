package raftnode

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"
	"github.com/steve-weiland/kvstore/internal/store"
)

// FSM implements raft.FSM on top of the KV store.
type FSM struct {
	store *store.Store
}

func newFSM(s *store.Store) *FSM { return &FSM{store: s} }

func (f *FSM) Apply(l *raft.Log) interface{} {
	cmd, err := decodeCommand(l.Data)
	if err != nil {
		return err
	}
	switch cmd.Op {
	case CmdPut:
		return f.store.Put(cmd.Key, cmd.Value)
	case CmdDel:
		return f.store.Delete(cmd.Key)
	default:
		return fmt.Errorf("unknown op: %s", cmd.Op)
	}
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	entries, err := f.store.Snapshot()
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{entries: entries}, nil
}

func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var entries map[string][]byte
	if err := json.NewDecoder(rc).Decode(&entries); err != nil {
		return err
	}
	return f.store.ReplaceContents(entries)
}

type fsmSnapshot struct {
	entries map[string][]byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.entries); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
