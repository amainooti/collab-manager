// Package api exposes the core.Service over REST. It is a thin transport
// layer — no business logic lives here, only request decoding, calling the
// service, and response encoding.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"outreach/internal/core"
)

type API struct {
	svc *core.Service
}

func New(svc *core.Service) *API {
	return &API{svc: svc}
}

// Register mounts routes on mux under /api/.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", a.listProjects)
	mux.HandleFunc("POST /api/projects", a.createProject)
	mux.HandleFunc("GET /api/projects/{id}", a.getProject)
	mux.HandleFunc("POST /api/projects/{id}/research", a.research)
	mux.HandleFunc("POST /api/projects/{id}/draft", a.draft)
	mux.HandleFunc("POST /api/projects/{id}/status", a.updateStatus)
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.svc.ListProjects()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Links string `json:"links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, errBadRequest("name is required"))
		return
	}
	p, err := a.svc.CreateProject(body.Name, body.Links)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) getProject(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	detail, err := a.svc.GetProjectDetail(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *API) research(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Refresh bool `json:"refresh"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional

	brief, err := a.svc.Research(r.Context(), id, body.Refresh)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, brief)
}

func (a *API) draft(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Goal    string `json:"goal"`
		Context string `json:"context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Goal == "" {
		writeErr(w, http.StatusBadRequest, errBadRequest("goal is required"))
		return
	}

	draft, err := a.svc.DraftPitch(r.Context(), id, body.Goal, body.Context)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (a *API) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Stage core.Stage `json:"stage"`
		Notes string     `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.svc.UpdateStatus(id, body.Stage, body.Notes); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct{ msg string }

func (e apiError) Error() string     { return e.msg }
func errBadRequest(msg string) error { return apiError{msg} }

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
