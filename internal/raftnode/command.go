package raftnode

import (
	"bytes"
	"encoding/gob"
)

// Command op constants — exposed so callers can pass them to Node.Apply.
const (
	CmdPut = "put"
	CmdDel = "del"
)

// Command is the payload committed through Raft.
type Command struct {
	Op    string
	Key   string
	Value []byte
}

func encodeCommand(c Command) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeCommand(data []byte) (Command, error) {
	var c Command
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&c); err != nil {
		return Command{}, err
	}
	return c, nil
}
