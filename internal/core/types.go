package core

import "time"

// Stage is a project's position in the outreach pipeline.
type Stage string

const (
	StageNew        Stage = "new"
	StageResearched Stage = "researched"
	StageReachedOut Stage = "reached_out"
	StageInTalks    Stage = "in_talks"
	StagePartnered  Stage = "partnered"
	StageDead       Stage = "dead"
)

var AllStages = []Stage{StageNew, StageResearched, StageReachedOut, StageInTalks, StagePartnered, StageDead}

type Project struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Links     string    `json:"links"` // newline-separated URLs
	Stage     Stage     `json:"stage"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Brief is the structured research output for a project. Cached one-per-project.
type Brief struct {
	ProjectID   int64     `json:"project_id"`
	Summary     string    `json:"summary"`
	Team        []string  `json:"team"`
	Socials     []string  `json:"socials"`
	RecentNews  []string  `json:"recent_news"`
	Tone        string    `json:"tone"`
	Sources     []string  `json:"sources"`
	Model       string    `json:"model"` // which LLM produced this, e.g. "claude-sonnet-5" or a HF model id
	GeneratedAt time.Time `json:"generated_at"`
}

type Draft struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"project_id"`
	Goal      string    `json:"goal"` // e.g. "intro", "partnership pitch", "follow-up"
	Content   string    `json:"content"`
	Model     string    `json:"model"` // which LLM produced this
	CreatedAt time.Time `json:"created_at"`
}

type HistoryEntry struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"project_id"`
	Stage     Stage     `json:"stage"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
}

// ProjectDetail bundles a project with everything the UI/API needs in one call.
type ProjectDetail struct {
	Project Project        `json:"project"`
	Brief   *Brief         `json:"brief,omitempty"`
	Drafts  []Draft        `json:"drafts"`
	History []HistoryEntry `json:"history"`
}
