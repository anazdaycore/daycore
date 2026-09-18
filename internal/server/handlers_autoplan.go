package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"daycore/internal/ai"
	"daycore/internal/domain"
	"daycore/internal/i18n"
	"daycore/internal/schedule"
)

func init() {
	// 逆操作与写入放在同一个文件 —— 改写入的人正好看得见它。
	// 自动规划复用整日 upsert 的逆操作 —— 撤销一次 auto-plan 就是把那天恢复成生成前的样子。
	registerRevert("plan_autoplan", (*Server).revertPlanUpsert)

	registerRoutes("ai", func(s *Server, mux Mux) {
		mux.HandleFunc("POST /api/ai/auto-plan", s.handleAIAutoPlan)
	})
}

var (
	// Weekday names go through the catalog rather than two parallel slices: a
	// third language was otherwise a code change and a release, which is the
	// one thing the message catalog exists to avoid.
	weekdayKeys = [7]string{
		i18n.Reg("weekday.sun", i18n.Text{"zh-CN": "星期日", "en-US": "Sunday"}),
		i18n.Reg("weekday.mon", i18n.Text{"zh-CN": "星期一", "en-US": "Monday"}),
		i18n.Reg("weekday.tue", i18n.Text{"zh-CN": "星期二", "en-US": "Tuesday"}),
		i18n.Reg("weekday.wed", i18n.Text{"zh-CN": "星期三", "en-US": "Wednesday"}),
		i18n.Reg("weekday.thu", i18n.Text{"zh-CN": "星期四", "en-US": "Thursday"}),
		i18n.Reg("weekday.fri", i18n.Text{"zh-CN": "星期五", "en-US": "Friday"}),
		i18n.Reg("weekday.sat", i18n.Text{"zh-CN": "星期六", "en-US": "Saturday"}),
	}
	// The one user-role line the planner turn opens with. Not a UI string and
	// not a template either — it is one sentence, and worker.go's morning and
	// evening openers are registered exactly this way.
	autoPlanUserText = i18n.Reg("autoplan.userOpener", i18n.Text{
		"zh-CN": "开始规划。",
		"en-US": "Generate the plan.",
	})
)

// POST /api/ai/auto-plan — the autonomous planner. Aggregates everything known
// about the session (course/rule commitments, assignment deadlines, grades,
// long-term memory) and generates a plan for one date (default today) or a
// short range, saved with SourceType "auto".
//
// Regeneration modes:
//   - "keep_manual" (default): rule blocks, manual/edited blocks, and completed
//     blocks survive; only previous auto blocks are regenerated.
//   - "replace_all": only rule blocks survive.
func (s *Server) handleAIAutoPlan(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if !s.requireAI(w, r) {
		return
	}
	if !s.rateLimit(w, r) {
		return
	}
	var body struct {
		clientClock
		From         string `json:"from"`
		To           string `json:"to"`
		Instructions string `json:"instructions"`
		Mode         string `json:"mode"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIAutoPlan.bad_request")
		return
	}
	mode := orDefault(body.Mode, "keep_manual")
	if mode != "keep_manual" && mode != "replace_all" {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIAutoPlan.bad_request2")
		return
	}

	locale := s.requestLocale(r)

	// Fill in "now" from the SESSION's clock when the client sent nothing.
	// ⚠️ This used to start at time.UTC and consult only the request field, so a
	// client that sends no zone had "today" drawn in UTC — the same defect the
	// companion's clock had, one layer down. dc is the same value the prompt is
	// built from further below, so the range and the prompt cannot disagree.
	dc := s.clockContext(r.Context(), sid, body.clientClock, locale)
	tz := dc.Timezone
	loc := resolveLocation(tz)
	now := time.Now().In(loc)
	if body.Date == "" {
		body.Date = now.Format("2006-01-02")
	}
	if body.Time == "" {
		body.Time = now.Format("15:04")
	}
	if body.Weekday == "" {
		if d, err := time.Parse("2006-01-02", body.Date); err == nil {
			body.Weekday = localeWeekday(locale, int(d.Weekday()))
		}
	}

	from, to := body.From, body.To
	if from == "" || to == "" {
		from, to = body.Date, body.Date
	}
	fromT, err1 := time.Parse("2006-01-02", from)
	toT, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || toT.Before(fromT) {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIAutoPlan.bad_request3")
		return
	}
	days := int(toT.Sub(fromT).Hours()/24) + 1
	if days > s.runtime().AutoPlanMaxDays {
		s.writeErrf(w, s.requestLocale(r), http.StatusBadRequest, "range_too_large",
			"err.aIAutoPlan.range_too_large", s.runtime().AutoPlanMaxDays)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.runtime().AIRequestTimeout)
	defer cancel()

	// ── gather materials ─────────────────────────────────────────────────
	occByDate := map[string][]domain.TimeBlock{}
	for _, b := range s.ruleOccurrences(ctx, sid, from, to) {
		occByDate[b.Date] = append(occByDate[b.Date], b)
	}
	storedByDate := map[string][]domain.TimeBlock{}
	noteByDate := map[string]*string{}
	if plans, err := s.store.DayPlans().Range(ctx, sid, from, to); err == nil {
		for _, p := range plans {
			storedByDate[p.Date] = p.Blocks
			noteByDate[p.Date] = p.Note
		}
	}

	// keptByDate is what survives regeneration (and gets persisted verbatim);
	// its visible subset is the "fixed commitments" context for the model.
	keptByDate := map[string][]domain.TimeBlock{}
	fixedForPrompt := []domain.TimeBlock{}
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		merged := schedule.Merge(storedByDate[date], occByDate[date])
		kept := make([]domain.TimeBlock, 0, len(merged))
		for _, b := range merged {
			switch {
			case b.RuleID != "": // rule occurrences (and their tombstones) always survive
				kept = append(kept, b)
			case mode == "keep_manual" && (b.Origin != domain.OriginAuto || b.Completed):
				kept = append(kept, b)
			}
		}
		keptByDate[date] = kept
		for _, b := range kept {
			if !b.Hidden {
				if b.Date == "" {
					b.Date = date
				}
				fixedForPrompt = append(fixedForPrompt, b)
			}
		}
	}

	horizon := toT.AddDate(0, 0, s.runtime().AssignmentLookaheadDays)
	dueFrom := now.Add(-24 * time.Hour)
	assignments, _ := s.store.Assignments().List(ctx, sid, domain.AssignmentFilter{DueFrom: &dueFrom, DueTo: &horizon})
	plannable := assignments[:0]
	for _, a := range assignments {
		if a.Status == domain.AssignmentDone || a.Status == domain.AssignmentDismissed {
			continue
		}
		plannable = append(plannable, a)
	}
	courses, _ := s.store.Courses().List(ctx, sid)

	// Long-term memory: server memory facts + legacy client-written key facts.
	factStrings := []string{}
	if facts, err := s.store.Memory().ListFacts(ctx, sid); err == nil {
		for _, f := range facts {
			factStrings = append(factStrings, f.Fact)
		}
	}
	if mem, err := s.store.Companion().Get(ctx, sid); err == nil {
		factStrings = append(factStrings, mem.KeyFacts...)
	}
	keyFacts := "[]"
	if len(factStrings) > 0 {
		keyFacts = marshalCompact(factStrings)
	}

	// ── render + call the model ──────────────────────────────────────────
	dc = s.clockContext(r.Context(), sid, body.clientClock, s.requestLocale(r))
	sys, err := s.prompts.Render(ctx, ai.PromptAutoPlan, locale, ai.AutoPlanData{
		Date: dc.Date, Weekday: dc.Weekday, Time: dc.Time, Timezone: dc.Timezone,
		RelativeDateMap: dc.RelativeDateMap,
		From:            from, To: to,
		Dates:        rangeDatesMarkdown(locale, fromT, toT),
		FixedBlocks:  marshalCompact(promptBlocks(fixedForPrompt)),
		Assignments:  assignmentsMarkdown(plannable, courses),
		Courses:      coursesMarkdown(courses),
		KeyFacts:     keyFacts,
		Instructions: strings.TrimSpace(body.Instructions),
	})
	if err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aIAutoPlan.internal")
		return
	}

	userMsg := i18n.T(autoPlanUserText, locale)
	planner := s.catalog.Planner()
	start := time.Now()
	resp, err := planner.Chat(ctx, ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: sys},
			{Role: ai.RoleUser, Content: userMsg},
		},
		Temperature: 0.3, MaxTokens: 8192, JSONMode: true,
	})
	s.logAICall(ctx, sid, epAutoPlan, planner.Model(), start, usageOf(resp), err)
	if err != nil {
		s.log.Error("ai auto-plan", "err", err)
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "server_error", "err.aIAutoPlan.server_error")
		return
	}
	result, ok := extractJSONObject(resp.Content)
	if !ok {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "parse_error", "err.aIAutoPlan.parse_error")
		return
	}
	if result["error"] != nil {
		// ⚠️ The MODEL's refusal, passed through verbatim — `no_material` is the
		// one the frontends branch on (api/FRONTEND_HANDOFF.md). Its message
		// comes from the prompt template, which is already per-locale, so it
		// does not belong in the catalog; the catalog covers text this package
		// writes, and this is text it forwards.
		s.writeJSON(w, http.StatusOK, result)
		return
	}

	// ── merge new auto blocks with survivors and persist ─────────────────
	newBlocks, warnings := parseAutoBlocks(result["blocks"], from, to, tz)
	note, _ := result["note"].(string)

	plans := []domain.DayPlan{}
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		blocks := append(keptByDate[date], newBlocks[date]...)
		old, existed := storedByDate[date]
		if len(blocks) == 0 && (!existed || len(old) == 0) {
			continue // nothing generated and nothing stale to clear
		}
		// An empty result must still be persisted when a stored plan exists:
		// skipping the upsert left the old auto blocks in place, which is the
		// opposite of what replace_all (and keep_manual on an all-auto day)
		// promised.
		sortBlocks(blocks)
		// Every regenerated date lands in the ledger, including first-time
		// plans (before=nil) and days the regeneration emptied — a write that
		// is not logged is a write that cannot be reverted, and "replan my
		// week" is exactly the operation a new user must be able to take back.
		s.logOp(ctx, &domain.OperationLog{
			SessionID: sid, Actor: domain.ActorAgent, Action: "plan_autoplan", Date: date,
			Summary: fmt.Sprintf("%s: replaced %d blocks", date, len(old)),
			Detail:  marshalCompact(map[string]any{"before": old}),
		})
		planNote := noteByDate[date]
		if note != "" {
			planNote = &note
		}
		saved, err := s.store.DayPlans().Upsert(ctx, &domain.DayPlan{
			SessionID: sid, Date: date, Blocks: blocks, SourceType: "auto", Note: planNote,
		})
		if err != nil {
			s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aIAutoPlan.internal2")
			return
		}
		saved.Blocks = schedule.Visible(saved.Blocks)
		plans = append(plans, *saved)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"plans": plans, "note": note, "mode": mode, "warnings": warnings,
	})
}

// parseAutoBlocks validates the model's blocks, stamps ids/origin, and groups
// them by date. Blocks dated outside [from, to] are dropped with a warning.
func parseAutoBlocks(v any, from, to, fallbackTZ string) (map[string][]domain.TimeBlock, []string) {
	byDate := map[string][]domain.TimeBlock{}
	var warnings []string
	arr, ok := v.([]any)
	if !ok {
		return byDate, warnings
	}
	raw, _ := json.Marshal(arr)
	var blocks []domain.TimeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return byDate, []string{"部分时间块无法解析，已忽略"}
	}
	base := time.Now().UnixNano()
	for i, b := range blocks {
		if b.Date < from || b.Date > to || !isDate(b.Date) {
			warnings = append(warnings, fmt.Sprintf("忽略了日期越界的时间块 %q（%s）", b.Title, b.Date))
			continue
		}
		if strings.TrimSpace(b.Title) == "" {
			continue
		}
		b.ID = fmt.Sprintf("block-%d-%d", base, i)
		b.Origin = domain.OriginAuto
		b.RuleID = ""
		b.Hidden = false
		b.Completed = false
		if !validBlockTypes[b.Type] {
			b.Type = domain.BlockTask
		}
		if b.TimeMode != domain.TimeFixed {
			b.TimeMode = domain.TimeFloating
		}
		if b.Timezone == "" {
			b.Timezone = fallbackTZ
		}
		byDate[b.Date] = append(byDate[b.Date], b)
	}
	return byDate, warnings
}

// promptBlocks strips blocks down to what the model needs to plan around.
func promptBlocks(blocks []domain.TimeBlock) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		m := map[string]any{"date": b.Date, "title": b.Title, "type": b.Type}
		if b.Time != nil {
			m["time"] = *b.Time
		}
		if b.DurationMin != nil {
			m["duration_min"] = *b.DurationMin
		}
		if b.Completed {
			m["completed"] = true
		}
		out = append(out, m)
	}
	return out
}

func rangeDatesMarkdown(locale string, from, to time.Time) string {
	var b strings.Builder
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		fmt.Fprintf(&b, "- %s（%s）\n", d.Format("2006-01-02"), localeWeekday(locale, int(d.Weekday())))
	}
	return b.String()
}

func localeWeekday(locale string, dow int) string {
	if dow < 0 || dow > 6 {
		return ""
	}
	return i18n.T(weekdayKeys[dow], locale)
}
