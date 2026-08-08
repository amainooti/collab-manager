// Package llmrouter composes a primary and a fallback core.LLM into a
// single core.LLM: try primary, and on any failure — rate limit, quota
// exhausted, outage, or simply not configured — fall through to the
// fallback and log which one actually served the request. Neither provider
// package needs to know the other exists.
package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"log"

	"outreach/internal/core"
)

type Router struct {
	primary  core.LLM
	fallback core.LLM
}

// New builds a router. Either argument may be nil — a nil primary skips
// straight to fallback (e.g. no ANTHROPIC_API_KEY configured), a nil
// fallback just means primary's own error is returned untouched. Passing
// both nil is a caller error, not something this package should silently
// tolerate, so ResearchProject/DraftPitch return an explicit error in that
// case rather than panicking later.
func New(primary, fallback core.LLM) *Router {
	return &Router{primary: primary, fallback: fallback}
}

var errNoProvider = errors.New("llmrouter: no model provider configured (set ANTHROPIC_API_KEY and/or HF_TOKEN)")

func (r *Router) ResearchProject(ctx context.Context, name, links string) (core.Brief, error) {
	if r.primary != nil {
		brief, err := r.primary.ResearchProject(ctx, name, links)
		if err == nil {
			return brief, nil
		}
		if r.fallback == nil {
			return core.Brief{}, err
		}
		log.Printf("llmrouter: primary research failed (%v), falling back", err)
	}
	if r.fallback == nil {
		return core.Brief{}, errNoProvider
	}
	brief, err := r.fallback.ResearchProject(ctx, name, links)
	if err != nil {
		return core.Brief{}, fmt.Errorf("fallback also failed: %w", err)
	}
	return brief, nil
}

func (r *Router) DraftPitch(ctx context.Context, projectName string, brief core.Brief, goal, extraContext string) (string, string, error) {
	if r.primary != nil {
		content, model, err := r.primary.DraftPitch(ctx, projectName, brief, goal, extraContext)
		if err == nil {
			return content, model, nil
		}
		if r.fallback == nil {
			return "", "", err
		}
		log.Printf("llmrouter: primary draft failed (%v), falling back", err)
	}
	if r.fallback == nil {
		return "", "", errNoProvider
	}
	content, model, err := r.fallback.DraftPitch(ctx, projectName, brief, goal, extraContext)
	if err != nil {
		return "", "", fmt.Errorf("fallback also failed: %w", err)
	}
	return content, model, nil
}
