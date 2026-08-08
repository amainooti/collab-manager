package core

import (
	"context"
	"fmt"
	"time"
)

// LLM is the interface any model provider satisfies — anthropicx.Client,
// hfx.Client, and llmrouter.Router (which composes the two with fallback)
// all implement this. Defined here (consumer side) so core has no
// import-time dependency on any specific provider's SDK.
//
// DraftPitch returns the model identifier alongside the content so callers
// can tell which provider actually produced a given draft — useful once a
// fallback is in play.
type LLM interface {
	ResearchProject(ctx context.Context, name, links string) (Brief, error)
	DraftPitch(ctx context.Context, projectName string, brief Brief, goal, extraContext string) (content, model string, err error)
}

// DB is the interface store.Store satisfies.
type DB interface {
	CreateProject(name, links string) (Project, error)
	GetProject(id int64) (Project, error)
	ListProjects() ([]Project, error)
	FindProjects(query string) ([]Project, error)
	UpdateStage(projectID int64, stage Stage, notes string) error
	SaveBrief(b Brief) error
	GetBrief(projectID int64) (*Brief, error)
	SaveDraft(projectID int64, goal, content, model string) (Draft, error)
	ListDrafts(projectID int64) ([]Draft, error)
	ListHistory(projectID int64) ([]HistoryEntry, error)
}

// Service is the "core engine": three functions — Research, DraftPitch,
// UpdateStatus — callable from REST, the web UI, or an MCP server. This is
// the one place that composes storage with the LLM.
type Service struct {
	DB  DB
	LLM LLM
}

func New(db DB, llm LLM) *Service {
	return &Service{DB: db, LLM: llm}
}

// CreateProject adds a new project in the "new" stage.
func (s *Service) CreateProject(name, links string) (Project, error) {
	return s.DB.CreateProject(name, links)
}

func (s *Service) ListProjects() ([]Project, error) {
	return s.DB.ListProjects()
}

// FindProjects searches projects by name (case-insensitive substring).
func (s *Service) FindProjects(query string) ([]Project, error) {
	return s.DB.FindProjects(query)
}

// GetProject fetches a single project without its brief/drafts/history —
// callers that only need to resolve an ID to a name (e.g. the Telegram
// bot) don't need the heavier GetProjectDetail.
func (s *Service) GetProject(id int64) (Project, error) {
	return s.DB.GetProject(id)
}

func (s *Service) GetProjectDetail(id int64) (ProjectDetail, error) {
	p, err := s.DB.GetProject(id)
	if err != nil {
		return ProjectDetail{}, err
	}
	brief, err := s.DB.GetBrief(id)
	if err != nil {
		return ProjectDetail{}, err
	}
	drafts, err := s.DB.ListDrafts(id)
	if err != nil {
		return ProjectDetail{}, err
	}
	history, err := s.DB.ListHistory(id)
	if err != nil {
		return ProjectDetail{}, err
	}
	return ProjectDetail{Project: p, Brief: brief, Drafts: drafts, History: history}, nil
}

// Research pulls what a project does, team/socials, recent activity, and
// tone into a structured brief, and caches it. If a brief already exists
// and refresh is false, the cached brief is returned without calling the
// LLM again.
func (s *Service) Research(ctx context.Context, projectID int64, refresh bool) (Brief, error) {
	p, err := s.DB.GetProject(projectID)
	if err != nil {
		return Brief{}, err
	}

	if !refresh {
		if cached, err := s.DB.GetBrief(projectID); err == nil && cached != nil {
			return *cached, nil
		}
	}

	brief, err := s.LLM.ResearchProject(ctx, p.Name, p.Links)
	if err != nil {
		return Brief{}, fmt.Errorf("research %s: %w", p.Name, err)
	}
	brief.ProjectID = projectID
	brief.GeneratedAt = time.Now().UTC()

	if err := s.DB.SaveBrief(brief); err != nil {
		return Brief{}, err
	}

	// Research implies at least "looked into it" — advance New -> Researched,
	// but don't clobber further-along stages.
	if p.Stage == StageNew {
		if err := s.DB.UpdateStage(projectID, StageResearched, "auto: research completed"); err != nil {
			return Brief{}, err
		}
	}

	return brief, nil
}

// DraftPitch drafts an outreach message for the given goal, using the
// cached brief (researching first if one doesn't exist yet) plus any
// extra context/voice guidance the caller supplies.
func (s *Service) DraftPitch(ctx context.Context, projectID int64, goal, extraContext string) (Draft, error) {
	p, err := s.DB.GetProject(projectID)
	if err != nil {
		return Draft{}, err
	}

	brief, err := s.DB.GetBrief(projectID)
	if err != nil {
		return Draft{}, err
	}
	if brief == nil {
		researched, err := s.Research(ctx, projectID, false)
		if err != nil {
			return Draft{}, fmt.Errorf("auto-research before draft: %w", err)
		}
		brief = &researched
	}

	content, model, err := s.LLM.DraftPitch(ctx, p.Name, *brief, goal, extraContext)
	if err != nil {
		return Draft{}, fmt.Errorf("draft pitch for %s: %w", p.Name, err)
	}

	return s.DB.SaveDraft(projectID, goal, content, model)
}

// UpdateStatus moves a project to a new stage and logs a history entry.
// Notes may be empty.
func (s *Service) UpdateStatus(projectID int64, stage Stage, notes string) error {
	if err := validateStage(stage); err != nil {
		return err
	}
	return s.DB.UpdateStage(projectID, stage, notes)
}

func validateStage(stage Stage) error {
	for _, s := range AllStages {
		if s == stage {
			return nil
		}
	}
	return fmt.Errorf("invalid stage %q", stage)
}
