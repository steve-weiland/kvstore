package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/sweiland/kvstore/internal/store"
)

const maxValueBytes = 64 * 1024

// Store is the interface the HTTP layer requires from the storage layer.
type Store interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Delete(key string) error
}

type Server struct {
	store  Store
	mux    *http.ServeMux
	startT time.Time
}

func New(s Store) *Server {
	srv := &Server{store: s, mux: http.NewServeMux(), startT: time.Now()}
	srv.mux.HandleFunc("GET /keys/{key}", srv.handleGet)
	srv.mux.HandleFunc("PUT /keys/{key}", srv.handlePut)
	srv.mux.HandleFunc("DELETE /keys/{key}", srv.handleDelete)
	srv.mux.HandleFunc("GET /health", srv.handleHealth)
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	val, err := s.store.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(val)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		jsonError(w, "key must not be empty", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxValueBytes+1))
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxValueBytes {
		jsonError(w, "value exceeds maximum size", http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.store.Put(key, body); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.store.Delete(key); errors.Is(err, store.ErrNotFound) {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	} else if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": int(time.Since(s.startT).Seconds()),
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
