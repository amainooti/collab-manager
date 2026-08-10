// Package anthropicx wraps the Anthropic Go SDK for the two LLM-backed core
// functions: researching a project (Claude Sonnet + web search) and drafting
// a pitch in the user's voice. Prompt text lives in internal/llmshared so it
// stays identical to the internal/hfx fallback provider.
package anthropicx

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"outreach/internal/core"
	"outreach/internal/llmshared"
)

// ResearchModel is deliberately Sonnet, not Opus — research is a
// high-volume, moderate-difficulty task (read pages, summarize) that
// doesn't need Opus-tier reasoning, and Sonnet is materially cheaper for
// something that runs on every new project.
const ResearchModel = "claude-sonnet-5"

// DraftModel matches the research model for consistency; swap independently
// if drafting quality needs a bump later.
const DraftModel = "claude-sonnet-5"

type Client struct {
	api anthropic.Client
}

// New creates a client using ANTHROPIC_API_KEY (or an `ant auth login`
// profile) from the environment.
func New() *Client {
	return &Client{api: anthropic.NewClient()}
}

// ResearchProject runs a web-search-backed research pass on a project and
// returns a structured brief: what it does, team/socials, recent activity,
// and tone. Two model calls: one to actually search and read, one to shape
// the findings into JSON matching core.Brief.
func (c *Client) ResearchProject(ctx context.Context, name, links string) (core.Brief, error) {
	findings, sources, err := c.gatherFindings(ctx, name, links)
	if err != nil {
		return core.Brief{}, fmt.Errorf("gather findings: %w", err)
	}

	brief, err := c.structureBrief(ctx, name, findings)
	if err != nil {
		return core.Brief{}, fmt.Errorf("structure brief: %w", err)
	}
	brief.Sources = sources
	brief.Model = ResearchModel
	return brief, nil
}

func (c *Client) gatherFindings(ctx context.Context, name, links string) (findings string, sources []string, err error) {
	system := []anthropic.TextBlockParam{{Text: llmshared.ResearchSystemPrompt(true)}}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(llmshared.ResearchUserPrompt(name, links))),
	}

	tools := []anthropic.ToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{}},
	}

	// Server-side tools resolve within the same request; a resume is only
	// needed if the server hits its internal search-iteration cap.
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     ResearchModel,
			MaxTokens: 8000,
			System:    system,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return "", nil, err
		}

		var text strings.Builder
		for _, block := range resp.Content {
			if v, ok := block.AsAny().(anthropic.TextBlock); ok {
				text.WriteString(v.Text)
				text.WriteString("\n")
			}
		}

		if resp.StopReason != anthropic.StopReasonPauseTurn {
			full := text.String()
			return full, llmshared.ExtractURLs(full), nil
		}

		// Resume: server paused after hitting its search-round cap.
		messages = append(messages, resp.ToParam())
	}

	return "", nil, fmt.Errorf("research did not complete after retries")
}

// structureBrief asks the model to turn free-text findings into strict
// JSON. Prompted-JSON rather than output_config.format so this doesn't
// depend on an unverified struct shape in the Go SDK; parse failures retry
// once with the error appended.
func (c *Client) structureBrief(ctx context.Context, name, findings string) (core.Brief, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		prompt := llmshared.StructureBriefPrompt(name, findings, lastErr)

		resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     DraftModel,
			MaxTokens: 4000,
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
			},
		})
		if err != nil {
			return core.Brief{}, err
		}

		var text strings.Builder
		for _, block := range resp.Content {
			if v, ok := block.AsAny().(anthropic.TextBlock); ok {
				text.WriteString(v.Text)
			}
		}

		brief, err := llmshared.ParseBriefJSON(text.String())
		if err != nil {
			lastErr = err
			continue
		}
		return brief, nil
	}

	return core.Brief{}, fmt.Errorf("model did not return parseable JSON: %w", lastErr)
}

// DraftPitch writes an outreach message from a research brief, a goal
// ("intro", "partnership pitch", "follow-up"), and optional free-text
// context (e.g. a voice sample or specific ask) supplied by the caller.
func (c *Client) DraftPitch(ctx context.Context, projectName string, brief core.Brief, goal, extraContext string) (string, string, error) {
	system := []anthropic.TextBlockParam{{Text: llmshared.DraftSystemPrompt}}

	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     DraftModel,
		MaxTokens: 2000,
		System:    system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(llmshared.DraftUserPrompt(projectName, brief, goal, extraContext))),
		},
	})
	if err != nil {
		return "", "", err
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if v, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(v.Text)
		}
	}
	return strings.TrimSpace(out.String()), DraftModel, nil
}

// GenerateCollab produces one piece of collaboration-meeting prep (intro,
// question list, follow-up, or closing) from a research brief and, for
// CollabFollowUp, pasted conversation notes.
func (c *Client) GenerateCollab(ctx context.Context, projectName string, brief core.Brief, mode core.CollabMode, conversationContext string) (string, string, error) {
	system := []anthropic.TextBlockParam{{Text: llmshared.CollabSystemPrompt(mode)}}

	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     DraftModel,
		MaxTokens: 2000,
		System:    system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(llmshared.CollabUserPrompt(projectName, brief, conversationContext))),
		},
	})
	if err != nil {
		return "", "", err
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if v, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(v.Text)
		}
	}
	return strings.TrimSpace(out.String()), DraftModel, nil
}
