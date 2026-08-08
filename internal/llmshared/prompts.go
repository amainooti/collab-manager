// Package llmshared holds the prompt text and response-parsing logic shared
// by every model provider (internal/anthropicx, internal/hfx, and any
// future one). Providers differ in how they call a model — tool use,
// streaming, auth — not in what they ask it to do, so that part lives here
// once instead of drifting between two near-identical copies.
package llmshared

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"outreach/internal/core"
)

// ResearchSystemPrompt returns the system prompt for the research pass.
// withWebSearch tailors the wording: providers with a live web-search tool
// (Claude) are told to search; providers without one (the Hugging Face
// fallback) are told plainly that they're working from training data and to
// flag that limitation rather than inventing specifics.
func ResearchSystemPrompt(withWebSearch bool) string {
	base := "You are a research assistant preparing a brief on a project/company ahead of an outreach message. " +
		"Find: what the project does, who the team is, their social/community links, recent activity " +
		"(launches, funding, posts, releases in the last few months), and the tone/voice they use publicly " +
		"(formal, casual, technical, meme-heavy, etc)."
	if withWebSearch {
		return base + " Search the web to find this. Be concrete and cite what you find; note when something " +
			"is uncertain rather than guessing. End your response with a 'Sources:' section listing every URL " +
			"you used, one per line."
	}
	return base + " You do NOT have web access — answer from what you already know plus any links given below. " +
		"If you don't have reliable knowledge of this specific project, say so plainly instead of inventing " +
		"details; a short honest brief beats a confident wrong one."
}

func ResearchUserPrompt(name, links string) string {
	userText := fmt.Sprintf("Project: %s", name)
	if strings.TrimSpace(links) != "" {
		userText += fmt.Sprintf("\nKnown links:\n%s", links)
	}
	return userText
}

// BriefJSONSchema describes the exact object shape the structuring step
// must return.
const BriefJSONSchema = `{"summary": string, "team": [string], "socials": [string], "recent_news": [string], "tone": string}`

// StructureBriefPrompt asks a model to turn free-text findings into the
// brief JSON shape. Used as a second pass after ResearchSystemPrompt, or
// combined into one prompt for providers without a search tool.
func StructureBriefPrompt(name, findings string, retryErr error) string {
	prompt := fmt.Sprintf(
		"Turn the research findings below into a single JSON object with exactly these fields: %s. "+
			"Respond with ONLY the JSON object — no markdown fences, no prose before or after.\n\n"+
			"Project: %s\n\nFindings:\n%s",
		BriefJSONSchema, name, findings,
	)
	if retryErr != nil {
		prompt += fmt.Sprintf("\n\nYour previous response failed to parse as JSON: %v. Try again, JSON only.", retryErr)
	}
	return prompt
}

// ParseBriefJSON parses a model's JSON-object response into the Brief
// fields it's responsible for (Summary/Team/Socials/RecentNews/Tone) —
// ProjectID, Model, Sources, and GeneratedAt are set by the caller.
func ParseBriefJSON(text string) (core.Brief, error) {
	var parsed struct {
		Summary    string   `json:"summary"`
		Team       []string `json:"team"`
		Socials    []string `json:"socials"`
		RecentNews []string `json:"recent_news"`
		Tone       string   `json:"tone"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &parsed); err != nil {
		return core.Brief{}, err
	}
	return core.Brief{
		Summary:    parsed.Summary,
		Team:       parsed.Team,
		Socials:    parsed.Socials,
		RecentNews: parsed.RecentNews,
		Tone:       parsed.Tone,
	}, nil
}

// ExtractJSON strips a leading/trailing markdown code fence if the model
// added one despite being asked not to.
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

var urlPattern = regexp.MustCompile(`https?://[^\s)\]"'>]+`)

// ExtractURLs pulls deduplicated URLs out of free text, in first-seen
// order. Used to recover a source list from a research pass's prose.
func ExtractURLs(text string) []string {
	found := urlPattern.FindAllString(text, -1)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(found))
	for _, u := range found {
		u = strings.TrimRight(u, ".,;:")
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// DraftSystemPrompt is the system prompt for turning a brief into an
// outreach message. The sender is assumed non-technical, reaching out to a
// technical team — so a technical hook is required (it's what makes the
// message credible and non-generic) but it must be explained in plain
// language inline, not dropped as unexplained jargon, so the sender fully
// understands what they're saying and can send it with confidence without
// asking an engineer to translate it first.
const DraftSystemPrompt = "You draft short, specific outreach messages for a person doing business development / " +
	"partnerships who is not a developer and may not have a technical background, reaching out to a technical " +
	"team. No generic filler, no 'I hope this email finds you well'. Reference something concrete and technical " +
	"from the research brief so the recipient can tell this isn't a template — but explain that detail in plain " +
	"language within the message itself (what it does and why it's relevant to reach out about), not as " +
	"unexplained jargon. The sender should be able to read the message back and fully understand what they're " +
	"saying — if a term needs a developer to decode it, rephrase it in plain terms instead of using it bare. " +
	"Keep it to a few short paragraphs suitable for email or DM. Output only the message text, no subject line " +
	"unless asked, no commentary."

// DraftUserPrompt assembles the brief + goal + optional extra context into
// the user turn for the drafting call.
func DraftUserPrompt(projectName string, brief core.Brief, goal, extraContext string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\nGoal: %s\n\n", projectName, goal)
	if brief.Summary != "" {
		fmt.Fprintf(&b, "What they do: %s\n", brief.Summary)
	}
	if len(brief.Team) > 0 {
		fmt.Fprintf(&b, "Team: %s\n", strings.Join(brief.Team, ", "))
	}
	if len(brief.RecentNews) > 0 {
		fmt.Fprintf(&b, "Recent activity: %s\n", strings.Join(brief.RecentNews, "; "))
	}
	if brief.Tone != "" {
		fmt.Fprintf(&b, "Their public tone: %s\n", brief.Tone)
	}
	if strings.TrimSpace(extraContext) != "" {
		fmt.Fprintf(&b, "\nAdditional context / voice guidance from the sender:\n%s\n", extraContext)
	}
	fmt.Fprintf(&b, "\nDraft the message now.")
	return b.String()
}
