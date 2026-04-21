package store_test

import (
	"errors"
	"os"
	"testing"

	"github.com/sweiland/kvstore/internal/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	f, err := os.CreateTemp("", "kv-*.log")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := store.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutGet(t *testing.T) {
	s := openTemp(t)

	if err := s.Put("hello", []byte("world")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("got %q, want %q", got, "world")
	}
}

func TestGetMissing(t *testing.T) {
	s := openTemp(t)

	_, err := s.Get("missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	s := openTemp(t)

	_ = s.Put("k", []byte("v"))
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get("k")
	if !errors.Is(err, store.ErrNotFound) {

		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestPutOverwrites(t *testing.T) {
	s := openTemp(t)

	_ = s.Put("k", []byte("v1"))
	_ = s.Put("k", []byte("v2"))
	got, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("got %q, want %q", got, "v2")
	}
}

func TestReplayRebuildsIndex(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-*.log")
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s1, _ := store.Open(f.Name())
	_ = s1.Put("persistent", []byte("value"))

	s2, err := store.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get("persistent")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("got %q after replay, want %q", got, "value")
	}
}
