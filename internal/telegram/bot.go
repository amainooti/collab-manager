// Package telegram is the Telegram bot frontend for core.Service — the
// same research/draft_pitch/update_status functions the web UI and REST
// API use, reachable via chat commands from a phone.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"outreach/internal/core"
)

const helpText = `Outreach bot — commands:
/list — all projects
/find <text> — search projects by name
/new <name> — add a project
/research <id or name> — research a project (Claude + web search, falls back to a free model if Claude is unavailable)
/draft <id> <goal> — draft a pitch (goal: intro / partnership pitch / follow-up)
/status <id> <stage> [notes] — move a project's stage (new, researched, reached_out, in_talks, partnered, dead)

Research and draft take a couple of minutes — I'll message you again when they're done.`

const telegramMaxLen = 4096

type Bot struct {
	api     *tgbotapi.BotAPI
	svc     *core.Service
	allowed map[int64]bool
}

// New reads TELEGRAM_BOT_TOKEN and TELEGRAM_ALLOWED_USERS from the
// environment. Both are required — a bot with no allowlist would let
// anyone who finds it spend the same Claude/HF API quota as the web app,
// which is exactly the gap that had to be closed there.
func New(svc *core.Service) (*Bot, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}
	allowedRaw := os.Getenv("TELEGRAM_ALLOWED_USERS")
	if allowedRaw == "" {
		return nil, fmt.Errorf("TELEGRAM_ALLOWED_USERS not set — set it to your Telegram numeric user ID(s), " +
			"comma-separated, or anyone who finds this bot can spend your API quota")
	}
	allowed := map[int64]bool{}
	for _, s := range strings.Split(allowedRaw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_ALLOWED_USERS: invalid user id %q: %w", s, err)
		}
		allowed[id] = true
	}

	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}

	return &Bot{api: api, svc: svc, allowed: allowed}, nil
}

// Run long-polls for updates and dispatches commands until ctx is
// cancelled.
func (b *Bot) Run(ctx context.Context) error {
	log.Printf("telegram bot @%s running, %d allowed user(s)", b.api.Self.UserName, len(b.allowed))

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil || !update.Message.IsCommand() {
				continue
			}
			go b.handleCommand(update.Message)
		}
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	if msg.From == nil || !b.allowed[msg.From.ID] {
		b.reply(msg.Chat.ID, "Not authorized.")
		who := "unknown"
		if msg.From != nil {
			who = fmt.Sprintf("%d (%s)", msg.From.ID, msg.From.FirstName)
		}
		log.Printf("telegram: rejected /%s from unauthorized user %s", msg.Command(), who)
		return
	}

	args := strings.TrimSpace(msg.CommandArguments())
	switch msg.Command() {
	case "start", "help":
		b.reply(msg.Chat.ID, helpText)
	case "list":
		b.cmdList(msg.Chat.ID)
	case "find":
		b.cmdFind(msg.Chat.ID, args)
	case "new":
		b.cmdNew(msg.Chat.ID, args)
	case "research":
		b.cmdResearch(msg.Chat.ID, args)
	case "draft":
		b.cmdDraft(msg.Chat.ID, args)
	case "status":
		b.cmdStatus(msg.Chat.ID, args)
	default:
		b.reply(msg.Chat.ID, "Unknown command.\n\n"+helpText)
	}
}

func (b *Bot) cmdList(chatID int64) {
	projects, err := b.svc.ListProjects()
	if err != nil {
		b.reply(chatID, "Error: "+err.Error())
		return
	}
	if len(projects) == 0 {
		b.reply(chatID, "No projects yet. Add one with /new <name>.")
		return
	}
	var out strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&out, "#%d %s — %s\n", p.ID, p.Name, p.Stage)
	}
	b.reply(chatID, out.String())
}

func (b *Bot) cmdFind(chatID int64, query string) {
	if query == "" {
		b.reply(chatID, "Usage: /find <text>")
		return
	}
	projects, err := b.svc.FindProjects(query)
	if err != nil {
		b.reply(chatID, "Error: "+err.Error())
		return
	}
	if len(projects) == 0 {
		b.reply(chatID, "No projects match \""+query+"\".")
		return
	}
	var out strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&out, "#%d %s — %s\n", p.ID, p.Name, p.Stage)
	}
	b.reply(chatID, out.String())
}

func (b *Bot) cmdNew(chatID int64, name string) {
	if name == "" {
		b.reply(chatID, "Usage: /new <project name>")
		return
	}
	p, err := b.svc.CreateProject(name, "")
	if err != nil {
		b.reply(chatID, "Error: "+err.Error())
		return
	}
	b.reply(chatID, fmt.Sprintf("Added #%d %s. /research %d to get started.", p.ID, p.Name, p.ID))
}

func (b *Bot) cmdResearch(chatID int64, arg string) {
	id, name, err := b.resolveProject(chatID, arg)
	if err != nil {
		return // resolveProject already replied with the reason
	}
	b.reply(chatID, fmt.Sprintf("🔍 Researching %s — this can take a couple of minutes, I'll message you when it's done.", name))
	go func() {
		brief, err := b.svc.Research(context.Background(), id, false)
		if err != nil {
			b.reply(chatID, fmt.Sprintf("Research on %s failed: %v", name, err))
			return
		}
		b.reply(chatID, formatBrief(name, brief))
	}()
}

func (b *Bot) cmdDraft(chatID int64, args string) {
	idStr, rest, ok := splitFirstToken(args)
	if !ok {
		b.reply(chatID, "Usage: /draft <id> <goal>  (goal: intro / partnership pitch / follow-up)")
		return
	}
	id, convErr := strconv.ParseInt(idStr, 10, 64)
	if convErr != nil {
		b.reply(chatID, "First argument must be a numeric project ID — use /list or /find to look it up.")
		return
	}
	goal := strings.TrimSpace(rest)
	if goal == "" {
		b.reply(chatID, "Usage: /draft <id> <goal>  (goal: intro / partnership pitch / follow-up)")
		return
	}
	p, err := b.svc.GetProject(id)
	if err != nil {
		b.reply(chatID, fmt.Sprintf("No project #%d.", id))
		return
	}
	b.reply(chatID, fmt.Sprintf("✍️ Drafting a %s for %s...", goal, p.Name))
	go func() {
		draft, err := b.svc.DraftPitch(context.Background(), id, goal, "")
		if err != nil {
			b.reply(chatID, fmt.Sprintf("Draft for %s failed: %v", p.Name, err))
			return
		}
		b.reply(chatID, formatDraft(p.Name, draft))
	}()
}

func (b *Bot) cmdStatus(chatID int64, args string) {
	idStr, rest, ok := splitFirstToken(args)
	if !ok {
		b.reply(chatID, statusUsage)
		return
	}
	id, convErr := strconv.ParseInt(idStr, 10, 64)
	if convErr != nil {
		b.reply(chatID, "First argument must be a numeric project ID — use /list or /find to look it up.")
		return
	}
	stageStr, notes, _ := splitFirstToken(rest)
	if stageStr == "" {
		b.reply(chatID, statusUsage)
		return
	}
	if err := b.svc.UpdateStatus(id, core.Stage(stageStr), notes); err != nil {
		b.reply(chatID, "Error: "+err.Error())
		return
	}
	b.reply(chatID, fmt.Sprintf("#%d moved to %s.", id, stageStr))
}

const statusUsage = "Usage: /status <id> <stage> [notes]  (stage: new, researched, reached_out, in_talks, partnered, dead)"

// resolveProject accepts a numeric ID or a name search string. On empty,
// missing, or ambiguous input it replies to the user itself (with usage
// help, a not-found message, or a disambiguation list) and returns an
// error so the caller can bail without sending a second message.
func (b *Bot) resolveProject(chatID int64, arg string) (id int64, name string, err error) {
	if arg == "" {
		b.reply(chatID, "Give a project ID or name — try /list or /find <text> first.")
		return 0, "", errors.New("empty project reference")
	}
	if id, convErr := strconv.ParseInt(arg, 10, 64); convErr == nil {
		p, err := b.svc.GetProject(id)
		if err != nil {
			b.reply(chatID, fmt.Sprintf("No project #%d.", id))
			return 0, "", err
		}
		return p.ID, p.Name, nil
	}

	matches, err := b.svc.FindProjects(arg)
	if err != nil {
		b.reply(chatID, "Error: "+err.Error())
		return 0, "", err
	}
	switch len(matches) {
	case 0:
		b.reply(chatID, "No project matches \""+arg+"\". Try /list.")
		return 0, "", errors.New("no match")
	case 1:
		return matches[0].ID, matches[0].Name, nil
	default:
		var out strings.Builder
		out.WriteString("Multiple matches, use the ID:\n")
		for _, p := range matches {
			fmt.Fprintf(&out, "#%d %s — %s\n", p.ID, p.Name, p.Stage)
		}
		b.reply(chatID, out.String())
		return 0, "", errors.New("ambiguous")
	}
}

// splitFirstToken splits s on the first run of whitespace, returning
// ok=false only for an empty/all-whitespace input.
func splitFirstToken(s string) (first, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	parts := strings.SplitN(s, " ", 2)
	if len(parts) == 1 {
		return parts[0], "", true
	}
	return parts[0], strings.TrimSpace(parts[1]), true
}

func formatBrief(name string, brief core.Brief) string {
	var out strings.Builder
	fmt.Fprintf(&out, "✅ Research done: %s\n\n%s\n", name, brief.Summary)
	if len(brief.Team) > 0 {
		fmt.Fprintf(&out, "\nTeam: %s", strings.Join(brief.Team, ", "))
	}
	if len(brief.Socials) > 0 {
		fmt.Fprintf(&out, "\nSocials: %s", strings.Join(brief.Socials, ", "))
	}
	if len(brief.RecentNews) > 0 {
		fmt.Fprintf(&out, "\nRecent: %s", strings.Join(brief.RecentNews, "; "))
	}
	if brief.Tone != "" {
		fmt.Fprintf(&out, "\nTone: %s", brief.Tone)
	}
	if brief.Model != "" {
		fmt.Fprintf(&out, "\n\n(via %s)", brief.Model)
	}
	return out.String()
}

func formatDraft(projectName string, d core.Draft) string {
	msg := fmt.Sprintf("✅ Draft ready: %s (%s)\n\n%s", projectName, d.Goal, d.Content)
	if d.Model != "" {
		msg += fmt.Sprintf("\n\n(via %s)", d.Model)
	}
	return msg
}

// reply sends a plain-text message — no ParseMode, deliberately: brief and
// draft content comes from an LLM and can contain characters (*, _, [, ])
// that break Telegram's Markdown parser and fail the send entirely. Plain
// text always goes through.
func (b *Bot) reply(chatID int64, text string) {
	if len(text) > telegramMaxLen {
		text = text[:telegramMaxLen-20] + "\n... (truncated)"
	}
	if _, err := b.api.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("telegram: send to %d failed: %v", chatID, err)
	}
}
