package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"daycore/internal/ai"
)

func init() {
	registerRoutes("ai", func(s *Server, mux Mux) {
		mux.HandleFunc("POST /api/ai/plan-text", s.handleAIPlanText)
		mux.HandleFunc("POST /api/ai/plan-image", s.handleAIPlanImage)
		mux.HandleFunc("POST /api/ai/extract-schedule-image", s.handleAIExtractScheduleImage)
	})
}

// POST /api/ai/plan-text — natural language → structured day plan (JSON).
// clientClock is the wall-clock the planned-AI endpoints accept from a client.
//
// ⚠️ Named and embedded rather than repeated inline three times: the fill-in step
// below has to apply to all three identically, and three anonymous structs is how
// one of them quietly stops being filled.
type clientClock struct {
	Date     string `json:"date"`
	Weekday  string `json:"weekday"`
	Time     string `json:"time"`
	Timezone string `json:"timezone"`
}

// clockContext resolves the zone (session first, the client's hint folded in) and
// fills any missing wall-clock field from the session's own clock.
//
// ⚠️ The client owns the clock when it sends one — only the device knows which day
// the reader means. When it sends NOTHING, the old path handed BuildDateContext an
// empty date, and parseLocalDate resolves that to UTC's today: a reader at 20:00 in
// Chicago asked for "today" and was handed tomorrow, because in UTC it already was.
func (s *Server) clockContext(ctx context.Context, sid string, c clientClock, locale string) ai.DateContext {
	tz := s.aiClockTimezone(ctx, sid, c.Timezone)
	date, weekday, clock := c.Date, c.Weekday, c.Time
	if date == "" {
		now := time.Now().In(resolveLocation(tz))
		date, weekday, clock = now.Format("2006-01-02"), ai.WeekdayName(now, locale), now.Format("15:04")
	} else if weekday == "" {
		if d, err := time.Parse("2006-01-02", date); err == nil {
			weekday = ai.WeekdayName(d, locale)
		}
	}
	return ai.BuildDateContext(date, weekday, clock, tz, locale)
}

func (s *Server) handleAIPlanText(w http.ResponseWriter, r *http.Request) {
	// Session first, then rate limit. These three were rate-limited only, which
	// made the most expensive endpoints in the product — two of them vision —
	// reachable by anyone who could reach the host, spending the operator's model
	// budget with an IP bucket as the only brake. Neither the contract nor the
	// docs ever said that: openapi's global security applies (they do not declare
	// `security: []`) and AUTH.md's public list does not include them.
	//
	// Safe to add: `sid` appears nowhere in this file — none of the three ever
	// touched the session, so nothing depended on anonymous access.
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
		Description string `json:"description"`
		clientClock
		TargetDate    string `json:"targetDate"`
		TargetWeekday string `json:"targetWeekday"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIPlanText.bad_request")
		return
	}
	if strings.TrimSpace(body.Description) == "" {
		// ⚠️ 200, not 400, and the status is load-bearing: this is not a
		// malformed request, it is a request the model was asked to answer and
		// could not. The frontends branch on the `error` field of a 200 body —
		// see api/FRONTEND_HANDOFF.md. writeErrL emits byte-identical JSON to
		// the writeJSON literal that used to be here; the only thing that
		// changed is that the text can now be translated.
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "no_schedule_info", "err.aIPlanText.no_schedule_info")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.runtime().AIRequestTimeout)
	defer cancel()

	dc := s.clockContext(r.Context(), sid, body.clientClock, s.requestLocale(r))
	sys, err := s.prompts.Render(ctx, ai.PromptDayPlanText, s.requestLocale(r), ai.PlanTextData{
		Date: dc.Date, Weekday: dc.Weekday, Time: dc.Time, Timezone: dc.Timezone,
		TargetDate: body.TargetDate, TargetWeekday: orDefault(body.TargetWeekday, dc.Weekday),
		RelativeDateMap: dc.RelativeDateMap,
	})
	if err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aIPlanText.internal")
		return
	}

	provider := s.catalog.DefaultChat()
	start := time.Now()
	resp, err := provider.Chat(ctx, ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: sys},
			{Role: ai.RoleUser, Content: body.Description},
		},
		Temperature: 0.3, MaxTokens: 4096, JSONMode: true,
	})
	s.logAICall(ctx, sessionIDFrom(r.Context()), epAIPlan, provider.Model(), start, usageOf(resp), err)
	if err != nil {
		s.log.Error("ai plan-text", "err", err)
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "server_error", "err.aIPlanText.server_error")
		return
	}

	result, ok := extractJSONObject(resp.Content)
	if !ok {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "parse_error", "err.aIPlanText.parse_error")
		return
	}
	if result["error"] != nil {
		s.writeJSON(w, http.StatusOK, result)
		return
	}
	attachBlocks(result, orDefault(body.TargetDate, dc.Date))
	s.writeJSON(w, http.StatusOK, result)
}

// POST /api/ai/plan-image — image → structured day plan via the vision pipeline.
func (s *Server) handleAIPlanImage(w http.ResponseWriter, r *http.Request) {
	// Session first, then rate limit. These three were rate-limited only, which
	// made the most expensive endpoints in the product — two of them vision —
	// reachable by anyone who could reach the host, spending the operator's model
	// budget with an IP bucket as the only brake. Neither the contract nor the
	// docs ever said that: openapi's global security applies (they do not declare
	// `security: []`) and AUTH.md's public list does not include them.
	//
	// Safe to add: `sid` appears nowhere in this file — none of the three ever
	// touched the session, so nothing depended on anonymous access.
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
		ImageBase64 string `json:"imageBase64"`
		MimeType    string `json:"mimeType"`
		clientClock
		TargetDate string `json:"targetDate"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIPlanImage.bad_request")
		return
	}
	if body.ImageBase64 == "" {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "no_image", "err.aIPlanImage.no_image")
		return
	}
	if int64(len(body.ImageBase64))*3/4 > s.runtime().MaxImageBytes {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "image_too_large", "err.aIPlanImage.image_too_large")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.runtime().AIRequestTimeout)
	defer cancel()

	dc := s.clockContext(r.Context(), sid, body.clientClock, s.requestLocale(r))
	sys, err := s.prompts.Render(ctx, ai.PromptDayPlanImage, s.requestLocale(r), ai.PlanImageData{
		Date: dc.Date, Weekday: dc.Weekday, Time: dc.Time, Timezone: dc.Timezone,
	})
	if err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aIPlanImage.internal")
		return
	}

	mime := body.MimeType
	if mime == "" {
		mime = "image/jpeg"
	}
	content, err := s.vision.PlanFromImage(ctx, s.catalog.DefaultChat(), sys, body.ImageBase64, mime)
	if errors.Is(err, ai.ErrNoVisionModel) {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "vision_unavailable", "err.aIPlanImage.vision_unavailable")
		return
	}
	if err != nil {
		s.log.Error("ai plan-image", "err", err)
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "server_error", "err.aIPlanImage.server_error")
		return
	}

	result, ok := extractJSONObject(content)
	if !ok {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "parse_error", "err.aIPlanImage.parse_error")
		return
	}
	if result["error"] != nil {
		s.writeJSON(w, http.StatusOK, result)
		return
	}
	attachBlocks(result, orDefault(body.TargetDate, dc.Date))
	s.writeJSON(w, http.StatusOK, result)
}

// POST /api/ai/extract-schedule-image — weekly timetable screenshot → recurring
// rule candidates (returned for user confirmation; nothing is saved here — the
// frontend saves confirmed rules via POST /api/rules/batch).
func (s *Server) handleAIExtractScheduleImage(w http.ResponseWriter, r *http.Request) {
	// Session first, then rate limit. These three were rate-limited only, which
	// made the most expensive endpoints in the product — two of them vision —
	// reachable by anyone who could reach the host, spending the operator's model
	// budget with an IP bucket as the only brake. Neither the contract nor the
	// docs ever said that: openapi's global security applies (they do not declare
	// `security: []`) and AUTH.md's public list does not include them.
	//
	// Safe to add: `sid` appears nowhere in this file — none of the three ever
	// touched the session, so nothing depended on anonymous access.
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
		ImageBase64 string `json:"imageBase64"`
		MimeType    string `json:"mimeType"`
		clientClock
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.aIExtractScheduleImage.bad_request")
		return
	}
	if body.ImageBase64 == "" {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "no_image", "err.aIExtractScheduleImage.no_image")
		return
	}
	if int64(len(body.ImageBase64))*3/4 > s.runtime().MaxImageBytes {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "image_too_large", "err.aIExtractScheduleImage.image_too_large")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.runtime().AIRequestTimeout)
	defer cancel()

	dc := s.clockContext(r.Context(), sid, body.clientClock, s.requestLocale(r))
	sys, err := s.prompts.Render(ctx, ai.PromptScheduleExtractImage, s.requestLocale(r), ai.PlanImageData{
		Date: dc.Date, Weekday: dc.Weekday, Time: dc.Time, Timezone: dc.Timezone,
	})
	if err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.aIExtractScheduleImage.internal")
		return
	}

	mime := body.MimeType
	if mime == "" {
		mime = "image/jpeg"
	}
	content, err := s.vision.PlanFromImage(ctx, s.catalog.DefaultChat(), sys, body.ImageBase64, mime)
	if errors.Is(err, ai.ErrNoVisionModel) {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "vision_unavailable", "err.aIExtractScheduleImage.vision_unavailable")
		return
	}
	if err != nil {
		s.log.Error("ai extract-schedule-image", "err", err)
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "server_error", "err.aIExtractScheduleImage.server_error")
		return
	}

	result, ok := extractJSONObject(content)
	if !ok {
		s.writeErrL(w, s.requestLocale(r), http.StatusOK, "parse_error", "err.aIExtractScheduleImage.parse_error")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
