package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steve-weiland/kvstore/internal/server"
	"github.com/steve-weiland/kvstore/internal/store"
)

// fakeStore is an in-memory store used to isolate HTTP handler tests.
type fakeStore struct {
	m map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string][]byte{}} }

func (f *fakeStore) Get(key string) ([]byte, error) {
	v, ok := f.m[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) Put(key string, value []byte) error {
	f.m[key] = value
	return nil
}

func (f *fakeStore) Delete(key string) error {
	if _, ok := f.m[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.m, key)
	return nil
}

func TestHandlePutGet(t *testing.T) {
	srv := server.New(newFakeStore(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/keys/hello", strings.NewReader("world"))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT: got %d, want 204", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/keys/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "world" {
		t.Fatalf("GET body: got %q, want %q", body, "world")
	}
}

func TestHandleGetMissing(t *testing.T) {
	srv := server.New(newFakeStore(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/keys/missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

func TestHandleHealth(t *testing.T) {
	srv := server.New(newFakeStore(), nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}
