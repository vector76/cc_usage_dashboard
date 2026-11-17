package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/consumption"
	"github.com/vector76/cc_usage_dashboard/internal/folder"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Project and folder-assignment endpoints (docs/projects.md). Mutations go
// through requireJSONPOST, the same Content-Type CSRF gate every POST uses;
// DELETE carries no body and, not being a "simple" method, cannot be sent
// cross-origin without a CORS preflight this server never grants.

const maxProjectBody = 4 << 10

func (s *Server) registerProjectRoutes() {
	s.mux.HandleFunc("GET /api/projects", s.handleListProjects)
	s.mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	s.mux.HandleFunc("PATCH /api/projects/{id}", s.handleRenameProject)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)
	s.mux.HandleFunc("PUT /api/project-folders", s.handleAssignFolder)
	s.mux.HandleFunc("GET /api/folders", s.handleListFolders)
	s.mux.HandleFunc("GET /api/usage/by-project", s.handleUsageByProject)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeStoreError maps the store's sentinel errors to statuses; anything else
// is a 500 whose detail goes to the log, not the client.
func writeStoreError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrDuplicateName):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalidName), errors.Is(err, store.ErrInvalidFolder):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		slog.Error(op+" failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, op+" failed")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return id, true
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListProjects()
	if err != nil {
		writeStoreError(w, "list projects", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": ps})
}

type projectNameRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxProjectBody) {
		return
	}
	var req projectNameRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	p, err := s.store.CreateProject(req.Name)
	if err != nil {
		writeStoreError(w, "create project", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleRenameProject(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxProjectBody) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req projectNameRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.store.RenameProject(id, req.Name); err != nil {
		writeStoreError(w, "rename project", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProject(id); err != nil {
		writeStoreError(w, "delete project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAssignFolder serves PUT /api/project-folders
// {"folder": "...", "project_id": 3}; a null project_id unassigns.
func (s *Server) handleAssignFolder(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxProjectBody) {
		return
	}
	var req struct {
		Folder    string `json:"folder"`
		ProjectID *int64 `json:"project_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.store.AssignFolder(req.Folder, req.ProjectID); err != nil {
		writeStoreError(w, "assign folder", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// folderRow is one observed folder with its effective project.
// AssignedProjectID is set only when this exact folder is assigned; ProjectID
// is the project it actually counts toward, and Via the assigned folder that
// decided it (itself or an ancestor).
type folderRow struct {
	Folder            string    `json:"folder"`
	Events            int64     `json:"events"`
	CostUSD           float64   `json:"cost_usd"`
	EstimatedUSD      float64   `json:"estimated_usd"`
	FirstSeen         time.Time `json:"first_seen"`
	LastSeen          time.Time `json:"last_seen"`
	AssignedProjectID *int64    `json:"assigned_project_id"`
	ProjectID         *int64    `json:"project_id"`
	Via               string    `json:"via,omitempty"`
}

// handleListFolders serves GET /api/folders: every distinct folder seen in
// usage_events (all time), with cost and its effective project. Folder ""
// collects events with no folder recorded; it is always Unassigned.
func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.FolderStats()
	if err != nil {
		writeStoreError(w, "list folders", err)
		return
	}
	as, err := s.store.ListFolderAssignments()
	if err != nil {
		writeStoreError(w, "list folders", err)
		return
	}
	res := folder.NewResolver(as)

	rows := make([]folderRow, 0, len(stats))
	for _, st := range stats {
		row := folderRow{
			Folder: st.Folder, Events: st.Events, CostUSD: st.CostUSD,
			EstimatedUSD: st.EstimatedUSD, FirstSeen: st.FirstSeen, LastSeen: st.LastSeen,
		}
		if a, ok := res.Resolve(st.Folder); ok {
			pid := a.ProjectID
			row.ProjectID = &pid
			row.Via = a.Folder
			if folder.Key(a.Folder) == folder.Key(st.Folder) {
				row.AssignedProjectID = &pid
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": rows})
}

// handleUsageByProject serves GET /api/usage/by-project?b=...&b=...: cost per
// bucket per project. The repeated b parameters are the bucket edges as
// RFC3339 instants, computed by the client from local midnights (see
// consumption.ParseBoundaries). A bad boundary list is 400; nothing is
// echoed back.
func (s *Server) handleUsageByProject(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query()["b"]
	bounds, err := consumption.ParseBoundaries(raw)
	if err != nil {
		slog.Warn("usage by project: bad boundaries", "err", err, "count", len(raw))
		writeJSONError(w, http.StatusBadRequest, "invalid bucket boundaries")
		return
	}
	projects, err := s.store.ListProjects()
	if err != nil {
		writeStoreError(w, "usage by project", err)
		return
	}
	as, err := s.store.ListFolderAssignments()
	if err != nil {
		writeStoreError(w, "usage by project", err)
		return
	}
	rep, err := consumption.NewCalculator(s.store.DB()).ByProject(bounds, projects, as)
	if err != nil {
		writeStoreError(w, "usage by project", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
