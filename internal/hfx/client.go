// Package hfx is the free-tier fallback model provider, backed by Hugging
// Face's Inference Providers router (an OpenAI-compatible chat-completions
// endpoint that fronts a mix of free and paid open-source models). It
// implements the same core.LLM interface as internal/anthropicx and shares
// its prompt text via internal/llmshared, so llmrouter can swap between the
// two without either package knowing the other exists.
//
// There is no official Hugging Face Go SDK, so this talks to the HTTP API
// directly with encoding/json — the request/response shape is the standard
// OpenAI chat-completions schema HF documents at
// https://router.huggingface.co/v1/chat/completions.
package hfx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"outreach/internal/core"
	"outreach/internal/llmshared"
)

const (
	defaultBaseURL = "https://router.huggingface.co/v1/chat/completions"
	// A small, widely-mirrored instruct model available across most HF
	// Inference Providers' free tiers. Override with HF_MODEL if your
	// account has a preferred provider/model combination — HF model IDs
	// for the router take the form "org/model" or "org/model:provider".
	defaultModel = "meta-llama/Llama-3.1-8B-Instruct"
)

type Client struct {
	token   string
	model   string
	baseURL string
	http    *http.Client
}

// New reads HF_TOKEN, HF_MODEL, and HF_BASE_URL from the environment. A
// client with no token is inert — Configured() reports false and every
// call fails fast — so it's always safe to construct one and let the
// caller decide whether to wire it in.
func New() *Client {
	return &Client{
		token:   os.Getenv("HF_TOKEN"),
		model:   envOr("HF_MODEL", defaultModel),
		baseURL: envOr("HF_BASE_URL", defaultBaseURL),
		http:    &http.Client{Timeout: 90 * time.Second},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Configured reports whether an HF_TOKEN was supplied.
func (c *Client) Configured() bool { return c.token != "" }

// ModelID is the identifier this client tags briefs/drafts with.
func (c *Client) ModelID() string { return "hf:" + c.model }

// ResearchProject mirrors anthropicx.Client.ResearchProject's two-pass
// shape (gather findings, then structure to JSON) but without a web-search
// tool — llmshared.ResearchSystemPrompt(false) tells the model to work from
// its own knowledge and say so when it doesn't have any, rather than
// inventing specifics.
func (c *Client) ResearchProject(ctx context.Context, name, links string) (core.Brief, error) {
	if !c.Configured() {
		return core.Brief{}, fmt.Errorf("hfx: HF_TOKEN not set")
	}

	findings, err := c.complete(ctx, llmshared.ResearchSystemPrompt(false), llmshared.ResearchUserPrompt(name, links), 1500)
	if err != nil {
		return core.Brief{}, fmt.Errorf("hfx gather findings: %w", err)
	}

	var brief core.Brief
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		text, err := c.complete(ctx, "", llmshared.StructureBriefPrompt(name, findings, lastErr), 1200)
		if err != nil {
			return core.Brief{}, fmt.Errorf("hfx structure brief: %w", err)
		}
		brief, err = llmshared.ParseBriefJSON(text)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return core.Brief{}, fmt.Errorf("hfx: model did not return parseable JSON: %w", lastErr)
	}

	brief.Sources = llmshared.ExtractURLs(links) // whatever the caller already gave us; no search to find more
	brief.Model = c.ModelID()
	return brief, nil
}

// DraftPitch drafts an outreach message using the same prompts as the
// Claude implementation.
func (c *Client) DraftPitch(ctx context.Context, projectName string, brief core.Brief, goal, extraContext string) (string, string, error) {
	if !c.Configured() {
		return "", "", fmt.Errorf("hfx: HF_TOKEN not set")
	}
	content, err := c.complete(ctx, llmshared.DraftSystemPrompt, llmshared.DraftUserPrompt(projectName, brief, goal, extraContext), 800)
	if err != nil {
		return "", "", err
	}
	return content, c.ModelID(), nil
}

// GenerateCollab mirrors the Claude implementation using the same collab
// prompts from internal/llmshared.
func (c *Client) GenerateCollab(ctx context.Context, projectName string, brief core.Brief, mode core.CollabMode, conversationContext string) (string, string, error) {
	if !c.Configured() {
		return "", "", fmt.Errorf("hfx: HF_TOKEN not set")
	}
	content, err := c.complete(ctx, llmshared.CollabSystemPrompt(mode), llmshared.CollabUserPrompt(projectName, brief, conversationContext), 1200)
	if err != nil {
		return "", "", err
	}
	return content, c.ModelID(), nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	var messages []chatMessage
	if system != "" {
		messages = append(messages, chatMessage{Role: "system", Content: system})
	}
	messages = append(messages, chatMessage{Role: "user", Content: user})

	body, err := json.Marshal(chatRequest{Model: c.model, Messages: messages, MaxTokens: maxTokens})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("hfx: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("hfx: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("hfx: %s returned %d: %s", c.model, resp.StatusCode, truncate(string(data), 500))
	}

	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("hfx: unmarshal response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("hfx: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("hfx: empty response from %s", c.model)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
