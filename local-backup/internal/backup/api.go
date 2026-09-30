package backup

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// Server exposes the backup service over an HTTP/JSON, localhost-only API.
type Server struct {
	svc *Service
	mux *http.ServeMux
}

func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/backups", s.handleBackup)
	s.mux.HandleFunc("GET /v1/snapshots", s.handleList)
	s.mux.HandleFunc("GET /v1/snapshots/{id}", s.handleGet)
	s.mux.HandleFunc("GET /v1/snapshots/{id}/diag", s.handleDiag)
	s.mux.HandleFunc("POST /v1/restore", s.handleRestore)
	s.mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeError(w, http.StatusBadRequest, "missing or invalid 'path'")
		return
	}
	rep, err := s.svc.Backup(req.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// An incomplete snapshot is a successful API call with an honest body:
	// the client must inspect "complete".
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.svc.manifest.ListSnapshots()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if snaps == nil {
		snaps = []Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	snap, err := s.svc.manifest.GetSnapshot(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	entries, err := s.svc.manifest.EntriesOf(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap, "entries": entries})
}

func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	snap, missing, err := s.svc.Diagnose(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if missing == nil {
		missing = []MissingChunk{}
	}
	state := "healthy"
	if snap.State != StatusComplete {
		state = "not_final"
	}
	if len(missing) > 0 {
		state = "missing_chunks"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot":       snap,
		"verification":   state,
		"missing_chunks": missing,
	})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SnapshotID int64  `json:"snapshot_id"`
		Target     string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SnapshotID == 0 || req.Target == "" {
		writeError(w, http.StatusBadRequest, "missing 'snapshot_id' or 'target'")
		return
	}
	rep, err := s.svc.Restore(req.SnapshotID, req.Target)
	if err != nil {
		var unsafe *RestoreUnsafeError
		if errors.As(err, &unsafe) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":          err.Error(),
				"missing_chunks": unsafe.Missing,
				"hint":           "the target was not written; inspect GET /v1/snapshots/{id}/diag",
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot id")
		return 0, false
	}
	return id, true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
