// Package web renders the kanban board UI on top of core.Service — plain
// server-rendered HTML plus htmx for the two actions that call an LLM
// (research, draft). Those run in a background goroutine and the page
// polls a status endpoint and shows a progress bar while they're in
// flight — a plain synchronous form POST gave no feedback for what can be
// a multi-minute wait, so a user who navigated away had no way to tell the
// action was still running.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"outreach/internal/core"
	"outreach/internal/jobs"
)

//go:embed templates/*.html
var templateFS embed.FS

type Web struct {
	svc  *core.Service
	tpl  *template.Template
	jobs *jobs.Tracker
}

func New(svc *core.Service) *Web {
	funcs := template.FuncMap{
		"join": strings.Join,
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	return &Web{svc: svc, tpl: tpl, jobs: jobs.NewTracker()}
}

func (h *Web) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.board)
	mux.HandleFunc("POST /projects/new", h.createProject)
	mux.HandleFunc("GET /projects/{id}", h.projectDetail)
	mux.HandleFunc("POST /projects/{id}/research", h.startResearch)
	mux.HandleFunc("GET /projects/{id}/research/status", h.researchStatus)
	mux.HandleFunc("POST /projects/{id}/draft", h.startDraft)
	mux.HandleFunc("GET /projects/{id}/draft/status", h.draftStatus)
	mux.HandleFunc("POST /projects/{id}/collab", h.startCollab)
	mux.HandleFunc("GET /projects/{id}/collab/status", h.collabStatus)
	mux.HandleFunc("POST /projects/{id}/status", h.updateStatus)
}

type column struct {
	Label    core.Stage
	Projects []core.Project
}

func (h *Web) board(w http.ResponseWriter, r *http.Request) {
	projects, err := h.svc.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	byStage := map[core.Stage][]core.Project{}
	for _, p := range projects {
		byStage[p.Stage] = append(byStage[p.Stage], p)
	}

	var columns []column
	for _, s := range core.AllStages {
		columns = append(columns, column{Label: s, Projects: byStage[s]})
	}

	h.render(w, "board", map[string]any{"Title": "Board — Outreach Engine", "Columns": columns})
}

func (h *Web) createProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	p, err := h.svc.CreateProject(name, r.FormValue("links"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}

// pageData backs both the full project page and the two htmx partials
// (research-panel, draft-panel) — the partials only read the fields they
// need off it, so one loader serves all three.
type pageData struct {
	Title       string
	Detail      core.ProjectDetail
	Stages      []core.Stage
	CollabModes []core.CollabMode
	ResearchJob jobs.State
	DraftJob    jobs.State
	CollabJob   jobs.State
	// OOB is true only when research-panel is rendered as a standalone htmx
	// response (not embedded in the full page) — it tells the template to
	// also emit an out-of-band update for the stage badge in the page
	// header, since research completing can auto-advance the project's
	// stage and that badge lives outside the panel htmx swaps. Skipped on
	// the full-page render because the header badge there is already
	// freshly rendered from the same data — emitting it twice would just
	// duplicate the element on the page.
	OOB bool
}

func (h *Web) loadPageData(id int64) (pageData, error) {
	detail, err := h.svc.GetProjectDetail(id)
	if err != nil {
		return pageData{}, err
	}
	return pageData{
		Title:       detail.Project.Name + " — Outreach Engine",
		Detail:      detail,
		Stages:      core.AllStages,
		CollabModes: core.AllCollabModes,
		ResearchJob: h.jobs.Get(researchKey(id)),
		DraftJob:    h.jobs.Get(draftKey(id)),
		CollabJob:   h.jobs.Get(collabKey(id)),
	}, nil
}

func (h *Web) projectDetail(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.render(w, "project", data)
}

func researchKey(id int64) string { return fmt.Sprintf("research:%d", id) }
func draftKey(id int64) string    { return fmt.Sprintf("draft:%d", id) }
func collabKey(id int64) string   { return fmt.Sprintf("collab:%d", id) }

// startResearch kicks off research in a background goroutine (detached
// from the request's context — the HTTP response returns almost
// immediately, well before the LLM call finishes) and responds with the
// research-panel partial, which will render in its "running" state and
// start polling researchStatus.
func (h *Web) startResearch(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	refresh := r.FormValue("refresh") == "true"

	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	key := researchKey(id)
	msg := fmt.Sprintf("Researching %s — this can take a couple of minutes...", data.Detail.Project.Name)
	if h.jobs.Start(key, msg) {
		go func() {
			if _, err := h.svc.Research(context.Background(), id, refresh); err != nil {
				h.jobs.Fail(key, err)
			} else {
				h.jobs.Finish(key)
			}
		}()
	}

	h.renderResearchPanel(w, id)
}

func (h *Web) researchStatus(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.renderResearchPanel(w, id)
}

// renderResearchPanel renders the current state of a project's research
// job. A terminal state (done/error) is consumed — cleared from the
// tracker — right after being rendered once, so a later unrelated page
// load doesn't keep re-showing a stale result forever; this response still
// reflects it correctly since we captured job before clearing.
func (h *Web) renderResearchPanel(w http.ResponseWriter, id int64) {
	key := researchKey(id)
	job := h.jobs.Get(key)
	if job.Status == jobs.StatusDone || job.Status == jobs.StatusError {
		h.jobs.Clear(key)
	}
	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.ResearchJob = job
	data.OOB = true
	h.render(w, "research-panel", data)
}

func (h *Web) startDraft(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	goal := r.FormValue("goal")
	extraContext := r.FormValue("context")
	if goal == "" {
		http.Error(w, "goal is required", http.StatusBadRequest)
		return
	}

	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	key := draftKey(id)
	msg := fmt.Sprintf("Drafting a %s for %s...", goal, data.Detail.Project.Name)
	if h.jobs.Start(key, msg) {
		go func() {
			if _, err := h.svc.DraftPitch(context.Background(), id, goal, extraContext); err != nil {
				h.jobs.Fail(key, err)
			} else {
				h.jobs.Finish(key)
			}
		}()
	}

	h.renderDraftPanel(w, id)
}

func (h *Web) draftStatus(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.renderDraftPanel(w, id)
}

func (h *Web) renderDraftPanel(w http.ResponseWriter, id int64) {
	key := draftKey(id)
	job := h.jobs.Get(key)
	if job.Status == jobs.StatusDone || job.Status == jobs.StatusError {
		h.jobs.Clear(key)
	}
	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.DraftJob = job
	h.render(w, "draft-panel", data)
}

// startCollab kicks off collab-prep generation (intro/questions/follow_up/
// closing) in a background goroutine, same pattern as startDraft — the
// pasted "conversation" field only matters for follow_up but is harmless to
// send along for the other modes since GenerateCollab only uses it there.
func (h *Web) startCollab(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := core.CollabMode(r.FormValue("mode"))
	conversation := r.FormValue("conversation")
	if mode == "" {
		http.Error(w, "mode is required", http.StatusBadRequest)
		return
	}

	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	key := collabKey(id)
	msg := fmt.Sprintf("Generating %s for %s...", strings.ReplaceAll(string(mode), "_", " "), data.Detail.Project.Name)
	if h.jobs.Start(key, msg) {
		go func() {
			if _, err := h.svc.GenerateCollab(context.Background(), id, mode, conversation); err != nil {
				h.jobs.Fail(key, err)
			} else {
				h.jobs.Finish(key)
			}
		}()
	}

	h.renderCollabPanel(w, id)
}

func (h *Web) collabStatus(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.renderCollabPanel(w, id)
}

func (h *Web) renderCollabPanel(w http.ResponseWriter, id int64) {
	key := collabKey(id)
	job := h.jobs.Get(key)
	if job.Status == jobs.StatusDone || job.Status == jobs.StatusError {
		h.jobs.Clear(key)
	}
	data, err := h.loadPageData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.CollabJob = job
	h.render(w, "collab-panel", data)
}

func (h *Web) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stage := core.Stage(r.FormValue("stage"))
	if err := h.svc.UpdateStatus(id, stage, r.FormValue("notes")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// render executes the named template — a full page ("board", "project") or
// a standalone htmx partial ("research-panel", "draft-panel"); both are
// just named templates in the same set.
func (h *Web) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error rendering %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
