package server

import (
	"net/http"

	"daycore/internal/auth"
	"daycore/internal/domain"
	"daycore/internal/i18n"
)

func init() {
	registerRoutes("session", func(s *Server, mux Mux) {
		mux.HandleFunc("POST /api/session/init", s.handleSessionInit)
		mux.HandleFunc("POST /api/session/theme", s.handleSessionTheme)
		mux.HandleFunc("PATCH /api/session/settings", s.handleSessionSettings)
	})
}

// POST /api/session/init — get-or-create the anonymous session. The server owns
// the session id: if there is no valid signed cookie, a fresh random id is
// generated and set as an httpOnly cookie (fixes v1's guessable query-param id).
func (s *Server) handleSessionInit(w http.ResponseWriter, r *http.Request) {
	// Optional body: {"tokenInBody":true} asks for the signed session token in
	// the response for clients without a cookie jar (native apps). Best-effort
	// parse — the endpoint has always accepted an empty body.
	var body struct {
		TokenInBody bool   `json:"tokenInBody"`
		Timezone    string `json:"timezone"`
	}
	_ = s.readJSON(r, &body)
	sid := sessionIDFrom(r.Context())
	if sid == "" {
		newID, err := auth.NewSessionID()
		if err != nil {
			s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.sessionInit.internal")
			return
		}
		sid = newID
	}
	sess, err := s.store.Sessions().GetOrCreate(r.Context(), sid)
	if err != nil {
		s.log.Error("session init", "err", err)
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.sessionInit.internal2")
		return
	}
	// First contact is the cheapest place to learn the device's zone and the most
	// important one: every clock the server runs for this session is derived from
	// it — the cron entries that fire the morning brief, the petrify line, the
	// rhythm day key, the companion's own clock. A client that never sends it keeps
	// the deployment default, which is only correct for someone in the operator's
	// own zone.
	//
	// Best-effort and never fatal: a session with no zone is exactly as broken as
	// it was a moment ago (see session_timezone.go).
	s.noteClientTimezone(r.Context(), sid, body.Timezone)

	// First contact: adopt the browser's language so prompts and AI replies
	// match the user before they ever open settings.
	if sess.Language == "" {
		lang := s.localePair(r.Context(), sid).Resolve("", r.Header.Get("Accept-Language"))
		if updated, err := s.store.Sessions().Update(r.Context(), sid, domain.SessionUpdate{Language: &lang}); err == nil {
			sess = updated
		}
	}
	s.setSessionCookie(w, sid)
	if body.TokenInBody {
		// Embed so the session's top-level JSON shape stays unchanged for
		// existing clients that Object.assign the response.
		s.writeJSON(w, http.StatusOK, struct {
			*domain.Session
			SessionToken string `json:"sessionToken"`
		}{sess, s.cookies.Sign(sid)})
		return
	}
	s.writeJSON(w, http.StatusOK, s.sessionWithFamilyTheme(r, sess))
}

// POST /api/session/theme — record a theme switch and update current theme.
func (s *Server) handleSessionTheme(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Theme string `json:"theme"`
	}
	if err := s.readJSON(r, &body); err != nil || body.Theme == "" {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.sessionTheme.bad_request")
		return
	}
	ctx := r.Context()
	fam := s.familyFor(r)
	_ = s.store.ThemeLog().Add(ctx, sid, body.Theme, fam.ID) // audit log is best-effort
	if err := s.setCurrentTheme(ctx, sid, fam.ID, body.Theme); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.sessionTheme.internal")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "familyId": fam.ID})
}

// PATCH /api/session/settings — update assistant name, theme, language, and/or persona.
func (s *Server) handleSessionSettings(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var body struct {
		AssistantName *string `json:"assistantName"`
		CurrentTheme  *string `json:"currentTheme"`
		Language      *string `json:"language"`
		PersonaPrompt *string `json:"personaPrompt"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "bad_request", "err.sessionSettings.bad_request")
		return
	}
	if body.Language != nil {
		// language is which of the user's two they are reading in right now —
		// what the home page switch flips. It must be one of their pair; the
		// pair itself is changed through preferences, not here.
		//
		// Reads clamp silently (Pair.Resolve) because a stored value can go
		// stale when someone changes their pair. A write is the user actively
		// choosing, so a value outside their pair says so rather than quietly
		// becoming something else.
		lang := i18n.Normalize(*body.Language)
		if !s.localePair(r.Context(), sid).Has(lang) {
			s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "unsupported_locale", "err.sessionSettings.unsupported_locale")
			return
		}
		body.Language = &lang
	}
	if body.PersonaPrompt != nil && len([]rune(*body.PersonaPrompt)) > 2000 {
		s.writeErrL(w, s.requestLocale(r), http.StatusBadRequest, "too_long", "err.sessionSettings.too_long")
		return
	}
	// ⚠️ currentTheme is routed through setCurrentTheme rather than passed down
	// with the rest, because for a non-fallback family it does not live on the
	// session row at all. Sending it down with the others would write the
	// desktop's theme every time a phone renamed its assistant.
	fam := s.familyFor(r)
	if body.CurrentTheme != nil && fam.ID != domain.FallbackFamilyID {
		if err := s.setCurrentTheme(r.Context(), sid, fam.ID, *body.CurrentTheme); err != nil {
			s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.sessionSettings.internal")
			return
		}
		body.CurrentTheme = nil
	}
	sess, err := s.store.Sessions().Update(r.Context(), sid, domain.SessionUpdate{
		AssistantName: body.AssistantName,
		CurrentTheme:  body.CurrentTheme,
		Language:      body.Language,
		PersonaPrompt: body.PersonaPrompt,
	})
	if err != nil {
		s.writeErrL(w, s.requestLocale(r), http.StatusInternalServerError, "internal", "err.sessionSettings.internal")
		return
	}
	s.writeJSON(w, http.StatusOK, s.sessionWithFamilyTheme(r, sess))
}
