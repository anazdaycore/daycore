package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"daycore/internal/ai"
	"daycore/internal/domain"

	"daycore/internal/i18n"
	"github.com/google/uuid"
)

func init() {
	registerRoutes("ai", func(s *Server, mux Mux) {
		mux.HandleFunc("POST /api/ai/companion", s.handleAICompanion)
	})
}

// POST /api/ai/companion — the companion agent over SSE v2. Context is
// server-authoritative (clock, plans, memory, rules, assignments, moods).
// When threadId is set, history is loaded from the server-side chat thread;
// otherwise the client uploads conversationHistory (anonymous sessions).
func (s *Server) handleAICompanion(w http.ResponseWriter, r *http.Request) {
	// Authenticate before parsing. These read the session further down anyway, so
	// the check was never missing — only late, which let an unauthenticated caller
	// probe body validation and spend the parser. Auth first is also what makes
	// "every non-public route answers 401" a checkable invariant
	// (auth_surface_test.go) rather than a per-handler habit.
	if _, ok := s.requireSession(w, r); !ok {
		return
	}
	if !s.requireAI(w, r) {
		return
	}
	if !s.rateLimit(w, r) {
		return
	}
	var body struct {
		Message             string   `json:"message"`
		Timezone            string   `json:"timezone"`
		Location            string   `json:"location"`
		AssistantName       string   `json:"assistantName"`
		ThreadID            string   `json:"threadId"`
		AttachmentIDs       []string `json:"attachmentIds"`
		ConversationHistory []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"conversationHistory"`
	}
	// A message may be empty when it carries files: "上传即 intent" — dropping a
	// photo in with no words is a complete thing to say.
	if err := s.readJSON(r, &body); err != nil ||
		(strings.TrimSpace(body.Message) == "" && len(body.AttachmentIDs) == 0) {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aICompanion.bad_request")
		return
	}

	sid := sessionIDFrom(r.Context())
	// The device is the only thing that knows which zone it is in, so this is a
	// hint we accept rather than context we refuse — see session_timezone.go for
	// why that is not a hole in "never trust client-supplied context".
	s.noteClientLocation(r.Context(), sid, body.Location)
	// ⚠️ Every clock below is the SESSION's, resolved once, here — the request field
	// is a hint that has just been folded in, never the answer. See aiClockTimezone
	// for the two failures that closes: a client that sends no zone used to hand the
	// model UTC, and a client that sent one used to overrule the settings page.
	tz := s.aiClockTimezone(r.Context(), sid, body.Timezone)
	atts, err := s.resolveAttachments(r.Context(), sid, body.AttachmentIDs)
	if err != nil {
		s.writeAttachmentErr(w, r, "aICompanion", err)
		return
	}
	s.decisions.cancelForSession(sid) // a new message supersedes any pending card

	ctx, cancel := context.WithTimeout(r.Context(), s.runtime().AIRequestTimeout)
	defer cancel()

	locale := s.requestLocale(r)
	name := orDefault(body.AssistantName, s.cfg.DefaultAssistantName)
	var messages []ai.Message
	// When threadId is set, load history from the server-side chat thread;
	// otherwise fall back to client-uploaded conversationHistory (anonymous).
	if body.ThreadID != "" {
		var err error
		messages, err = s.buildCompanionMessages(ctx, sid, body.ThreadID, locale, tz, name, body.Message, nil)
		if err != nil {
			s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aICompanion.internal")
			return
		}
	} else {
		sys, err := s.companionSystemPrompt(ctx, sid, locale, tz, name)
		if err != nil {
			s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aICompanion.internal")
			return
		}
		messages = []ai.Message{{Role: ai.RoleSystem, Content: sys}}
		hist := body.ConversationHistory
		if len(hist) > 20 {
			hist = hist[len(hist)-20:]
		}
		for _, m := range hist {
			messages = append(messages, ai.Message{Role: safeRole(m.Role), Content: m.Content})
		}
		messages = append(messages, ai.Message{Role: ai.RoleUser, Content: body.Message})
		messages = s.maybeCompress(ctx, sid, "", locale, tz, name, messages)
	}
	attachPartsToLastUser(messages, s.attachmentParts(ctx, s.catalog.DefaultChat(), locale, atts))

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering for SSE
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	rc.Flush()

	answer := s.runCompanionAgent(ctx, sseSender{w: w, rc: rc}, r, s.catalog.DefaultChat(), sid, locale, tz, messages, true)

	// Persist the turn to the server-side thread so threadId multi-turn context
	// actually accumulates across requests. Messages carry sid, so a stray
	// threadId can't write into another session's thread. Best-effort: the SSE
	// stream already completed.
	if body.ThreadID != "" && strings.TrimSpace(answer) != "" {
		persistCtx := context.WithoutCancel(ctx)
		// The id is generated here, not read back from the append: mongostore's
		// AppendMessages does not write generated ids into the caller's slice,
		// so binding to turn[0].ID would attach nothing on exactly one of the
		// four backends — and only when a user sent a file.
		userMsgID := uuid.NewString()
		err := s.store.Chats().AppendMessages(persistCtx, []domain.ChatMessage{
			{ID: userMsgID, ThreadID: body.ThreadID, SessionID: sid, Role: domain.RoleUser, Content: body.Message},
			{ThreadID: body.ThreadID, SessionID: sid, Role: domain.RoleAssistant, Content: answer},
		})
		if err == nil {
			// A bind that fails leaves the uploads unbound and reclaimable —
			// see bindAttachments.
			if err := s.bindAttachments(persistCtx, sid, body.ThreadID, userMsgID, idsOf(atts)); err != nil {
				s.log.Warn("could not attach uploads to the message", "session", sid, "err", err)
			}
		}
	}
}

// buildCompanionMessages assembles system prompt + persisted thread summary +
// thread history + the new user message, then applies sliding-window
// compression — the threadId flow shared by the sync SSE endpoint and the
// async endpoint. exclude skips message IDs already persisted for the current
// turn (the async flow writes user + placeholder rows before running the
// agent); pending placeholders from concurrent runs are always skipped.
func (s *Server) buildCompanionMessages(ctx context.Context, sid, threadID, locale, tz, name, userMsg string, exclude map[string]bool) ([]ai.Message, error) {
	sys, err := s.companionSystemPrompt(ctx, sid, locale, tz, name)
	if err != nil {
		return nil, err
	}
	messages := []ai.Message{{Role: ai.RoleSystem, Content: sys}}
	// Re-inject the persisted rolling summary so compressed context carries
	// across requests (each request otherwise rebuilds only from raw messages).
	if threads, err := s.store.Chats().ListThreads(ctx, sid); err == nil {
		for _, th := range threads {
			if th.ID == threadID {
				if strings.TrimSpace(th.Summary) != "" {
					messages = append(messages, ai.Message{Role: ai.RoleSystem, Content: "[对话摘要]\n" + th.Summary})
				}
				break
			}
		}
	}
	if dbMsgs, err := s.store.Chats().ListMessages(ctx, threadID, sid, "", 50); err == nil {
		// Messages come newest-first; reverse to chronological order.
		for i := len(dbMsgs) - 1; i >= 0; i-- {
			if exclude[dbMsgs[i].ID] || dbMsgs[i].Status == domain.MsgStatusPending {
				continue
			}
			// ⚠️ safeRole here too, and its absence was a hole rather than an
			// oversight in a corner: a client could put `role: "system"` into
			// POST /api/companion-history, GET /api/chat/threads imports that
			// verbatim into chat_messages, and this line turned it back into a
			// real system turn in its own LLM context. The anonymous path above
			// has defended against exactly this since it was written; the
			// threaded path — the one every frontend actually uses — did not.
			messages = append(messages, ai.Message{Role: safeRole(dbMsgs[i].Role), Content: dbMsgs[i].Content})
		}
	}
	messages = append(messages, ai.Message{Role: ai.RoleUser, Content: userMsg})
	return s.maybeCompress(ctx, sid, threadID, locale, tz, name, messages), nil
}

// Section headings for the assembled system prompt. They are prompt structure
// rather than user-visible copy, but the model reads them — a Chinese heading
// over an English body is the kind of mixed signal that gets answered in the
// wrong language.
var (
	personaHeading = i18n.Reg("companion.personaHeading", i18n.Text{
		"zh-CN": "## 你的个性化风格设定",
		"en-US": "## Your personalized style",
	})
	wishPoolHeading = i18n.Reg("companion.wishPoolHeading", i18n.Text{
		"zh-CN": "## 愿望池（用户想做但还没安排的事）",
		"en-US": "## Wish pool (things the user wants to do but hasn't scheduled)",
	})
)

// companionSystemPrompt assembles the layered system prompt:
//
//	L1_hard (pure boundaries, zero personality)
//	→ L3_context (data: clock, plans, memory, weather)
//	→ L2_persona (role + style, default "good buddy" or user override)
//	→ L1_reminder (restated boundaries that L2 cannot override)
func (s *Server) companionSystemPrompt(ctx context.Context, sid, locale, tz, name string) (string, error) {
	// L1: pure rule list — no template variables needed.
	l1, err := s.prompts.Render(ctx, ai.PromptCompanionAgent, locale, ai.CompanionAgentData{})
	if err != nil {
		return "", err
	}

	// L3: data context (clock, plans, memory, weather, assignments, rules, moods).
	// ⚠️ resolveLocation keeps UTC as the LAST resort, and that is all it may be
	// now that the caller resolves the zone from the session instead of forwarding
	// whatever the client happened to send — see handleAICompanion. The old
	// "tz == "" → UTC" here was not a last resort, it was the common case: the
	// async companion's caller sends no zone at all (初版的陪伴走 /api/ai/companion/
	// async，而 core 的 askCompanion 没有 timezone 参数；长卷与纸屿走 streaming，会带上)。
	loc := resolveLocation(tz)
	now := time.Now().In(loc)
	date := now.Format("2006-01-02")
	weekday := ai.WeekdayName(now, locale)
	dc := ai.BuildDateContext(date, weekday, now.Format("15:04"), tz, locale)

	assignmentsCtx, rulesCtx := s.companionMaterials(ctx, sid)
	l3, err := s.prompts.Render(ctx, ai.PromptCompanionContext, locale, ai.CompanionContextData{
		Date: date, Weekday: weekday, Time: now.Format("15:04"), Timezone: tz,
		RelativeDateMap:    dc.RelativeDateMap,
		TodayPlan:          s.planBlocksJSON(ctx, sid, date),
		TomorrowPlan:       s.planBlocksJSON(ctx, sid, now.AddDate(0, 0, 1).Format("2006-01-02")),
		MemoryFacts:        s.memoryFactsContext(ctx, sid),
		AssignmentsContext: assignmentsCtx, RulesContext: rulesCtx,
		MoodHistory: s.moodHistoryContext(ctx, sid),
		// Built from the SAME usable set the tool band's enum came from, in the
		// same round. A description naming a source the model cannot call is
		// worse than no description: it invites a call that fails.
		Sources: s.sourceLines(locale),
	})
	if err != nil {
		return "", err
	}

	// L2: role layer — user override or built-in "good buddy" default.
	l2 := ""
	if sess, err := s.store.Sessions().Get(ctx, sid); err == nil && sess.PersonaPrompt != "" {
		l2 = "\n\n" + i18n.T(personaHeading, locale) + "\n" + sess.PersonaPrompt
	} else if persona, err := s.prompts.Render(ctx, ai.PromptPersona, locale, map[string]any{"Name": name}); err == nil {
		l2 = "\n\n" + persona
	}

	// L1_reminder: hard boundary restatement placed AFTER L2 so it can never
	// be overridden by "ignore previous instructions" attacks.
	reminder := "\n\n" + ai.HardBoundaryReminder(locale)

	// Append the active wish pool so the assistant can proactively suggest
	// something from it (the wish ↔ mood linkage).
	l3extra := ""
	if wishesCtx := s.activeWishesContext(ctx, sid); wishesCtx != "" && wishesCtx != "[]" {
		l3extra = "\n\n" + i18n.T(wishPoolHeading, locale) + "\n" + wishesCtx
	}

	return l1 + "\n\n" + l3 + l3extra + l2 + reminder, nil
}

// planBlocksJSON renders a date's visible blocks as compact JSON for prompt
// injection ("[]" when nothing is scheduled).
func (s *Server) planBlocksJSON(ctx context.Context, sid, date string) string {
	blocks := s.planBlocksForDate(ctx, sid, date)
	if len(blocks) == 0 {
		return "[]"
	}
	return marshalCompact(blocks)
}

// safeRole maps a stored role onto one this process is willing to send.
//
// ⚠️ A whitelist, and it must stay one. Everything that reaches it has been
// through a client: POST /api/companion-history takes `[]domain.Message` with a
// free-form Role and stores it verbatim, and GET /api/chat/threads imports that
// into chat_messages the same way. So "the role in the database" is client
// input wearing a database's clothes.
//
// Anything that is not explicitly assistant becomes a user turn. That is
// deliberately blunt: `system` is the one that matters (it is the instruction
// channel), but `tool` would also let a client fabricate a tool result the model
// treats as ground truth, and there is no third role worth preserving from a
// source we do not trust.
//
// One function rather than the same four lines twice, because the second copy is
// the one that gets forgotten — which is what happened.
func safeRole(raw string) ai.Role {
	if raw == string(ai.RoleAssistant) {
		return ai.RoleAssistant
	}
	return ai.RoleUser
}
