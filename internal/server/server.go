package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/steve-weiland/kvstore/internal/store"
)

const maxValueBytes = 64 * 1024

// Store is the read interface the HTTP layer requires from the storage layer.
type Store interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Delete(key string) error
}

// Applier routes mutating commands through a consensus layer.
// When nil, writes go directly to the Store (single-node mode without Raft).
type Applier interface {
	Apply(op, key string, value []byte) error
	IsLeader() bool
	// LeaderAddr returns the leader's HTTP address for client redirects ("" if unknown).
	LeaderAddr() string
	// Barrier blocks until the FSM has applied all log entries up to the current
	// commit index, guaranteeing that a subsequent Get reflects all committed writes.
	Barrier(timeout time.Duration) error
}

type Server struct {
	store   Store
	applier Applier
	mux     *http.ServeMux
	startT  time.Time
}

func New(s Store, a Applier) *Server {
	srv := &Server{store: s, applier: a, mux: http.NewServeMux(), startT: time.Now()}
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
	if s.applier != nil && r.URL.Query().Get("consistent") == "true" {
		if err := s.applier.Barrier(5 * time.Second); err != nil {
			jsonError(w, "barrier failed", http.StatusServiceUnavailable)
			return
		}
	}
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

	if err := s.applyWrite("put", key, body); err != nil {
		s.writeError(w, r, key, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.applyWrite("del", key, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonError(w, "key not found", http.StatusNotFound)
			return
		}
		s.writeError(w, r, key, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyWrite routes a write through Raft when an Applier is set, or directly
// to the store in single-node mode.
func (s *Server) applyWrite(op, key string, value []byte) error {
	if s.applier != nil {
		return s.applier.Apply(op, key, value)
	}
	if op == "put" {
		return s.store.Put(key, value)
	}
	return s.store.Delete(key)
}

// writeError handles errors from applyWrite, including leader-redirect for non-leaders.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, key string, err error) {
	if s.applier != nil && !s.applier.IsLeader() {
		if addr := s.applier.LeaderAddr(); addr != "" {
			http.Redirect(w, r, addr+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		jsonError(w, "not leader", http.StatusServiceUnavailable)
		return
	}
	jsonError(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"status":         "ok",
		"uptime_seconds": int(time.Since(s.startT).Seconds()),
	}
	if s.applier != nil {
		resp["raft_leader"] = s.applier.IsLeader()
		resp["leader_addr"] = s.applier.LeaderAddr()
	}
	json.NewEncoder(w).Encode(resp)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
