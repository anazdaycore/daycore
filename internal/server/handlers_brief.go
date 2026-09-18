package server

import (
	"errors"
	"net/http"
	"time"

	"daycore/internal/domain"
	"daycore/internal/i18n"
	"daycore/internal/schedule"
)

func init() {
	registerRoutes("brief", func(s *Server, mux Mux) {
		mux.HandleFunc("GET /api/brief", s.handleBrief)
	})
	i18n.Register("brief.empty.title", i18n.Text{
		"zh-CN": "今天还是空的", "en-US": "Today is still empty",
	})
	i18n.Register("brief.empty.line1", i18n.Text{
		"zh-CN": "不是坏事——说明还没人替你着急。", "en-US": "Not a bad thing — it means no one is rushing you yet.",
	})
	i18n.Register("brief.empty.line2", i18n.Text{
		"zh-CN": "跟我说一句今天有什么，我来搭起来；", "en-US": "Tell me one thing about today and I will build it up;",
	})
	i18n.Register("brief.empty.line3", i18n.Text{
		"zh-CN": "或者什么都不说，今天就这样也很好。", "en-US": "Or say nothing, and today is fine just as it is.",
	})
	i18n.Register("brief.line1", i18n.Text{
		"zh-CN": "%d 件事，其中 %d 件是定点的课。", "en-US": "%d things, %d of them fixed appointments.",
	})
	i18n.Register("brief.line3", i18n.Text{
		"zh-CN": "没做完的不积债——要紧的明天会自己浮上来。", "en-US": "Nothing unfinished becomes a debt — what matters surfaces again tomorrow.",
	})
	i18n.Register("brief.dayTitle", i18n.Text{
		"zh-CN": "今天 · %s", "en-US": "Today · %s",
	})
}

// GET /api/brief — the in-app morning导语, derived from today's plan. Read-only
// and deterministic: the mock's brief() (design-ui/core/daycode-core.js) builds
// the same two shapes from the day's blocks without a model call.
func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	locale := s.requestLocale(r)
	today := time.Now().In(s.sessionLocation(ctx, sid)).Format("2006-01-02")

	plan, err := s.store.DayPlans().Get(ctx, sid, today)
	if errors.Is(err, domain.ErrNotFound) {
		// ⚠️ No row yet is the NORMAL case, not an edge one: every account
		// starts with no rows, and so does every day before its first plan
		// write. Tolerating the error and then reading plan.Blocks panicked on
		// exactly that state — the empty branch below was unreachable and the
		// endpoint 500'd when it was supposed to be at its gentlest.
		plan = &domain.DayPlan{SessionID: sid, Date: today, SourceType: "rules"}
	} else if err != nil {
		s.writeErrL(w, locale, http.StatusInternalServerError, "internal", "err.brief.internal")
		return
	}
	blocks := schedule.Merge(plan.Blocks, s.ruleOccurrences(ctx, sid, today, today))
	s.normalizePlanBlocks(blocks, today, "", locale)
	blocks = schedule.Visible(blocks)

	if len(blocks) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"empty": true,
			"date":  today,
			"title": i18n.T("brief.empty.title", locale),
			"lines": []string{
				i18n.T("brief.empty.line1", locale),
				i18n.T("brief.empty.line2", locale),
				i18n.T("brief.empty.line3", locale),
			},
		})
		return
	}

	fixed := 0
	for _, b := range blocks {
		if b.Type == domain.BlockAppointment {
			fixed++
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"empty": false,
		"date":  today,
		"title": i18n.Tf("brief.dayTitle", locale, today),
		"lines": []string{
			i18n.Tf("brief.line1", locale, len(blocks), fixed),
			i18n.T("brief.line3", locale),
		},
	})
}
