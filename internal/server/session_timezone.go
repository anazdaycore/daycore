package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"daycore/internal/domain"
)

// Per-session timezone.
//
// # The defect this closes
//
// Every clock in the server used to read one deployment-wide value,
// WORKER_DEFAULT_TZ. For a user in another zone that is not a cosmetic offset —
// it moves things that are supposed to be anchored to their day:
//
//	the petrify line     their evening freezes early or late (plan_guard.go)
//	the rhythm day key   their "day" starts at somebody else's midnight (awake.go)
//	the morning brief    arrives at 07:30 in a city they do not live in
//	a daily occurrence   two briefs on one of their days, none on another
//
// # Why it lives in preferences and not in a column
//
// The repo's rule is "anything that appears in a WHERE clause, or is updated by
// arithmetic, must be a column". A timezone is read per session and never
// queried across sessions, so the preferences blob is the right home — same
// place, and for the same reason, as PrimaryLocale.
//
// # Where the value comes from, and why the client is allowed to tell us
//
// This repo has a standing rule that the companion handler does not accept
// context from the client: no todayPlan, no moodHistory, no date. A timezone
// looks like a violation and is not, and the distinction is worth stating
// because the next person will ask.
//
// That rule exists to stop a client supplying data the SERVER can derive — where
// accepting it means the client can lie about the user's own history. A timezone
// is not derivable server-side at all: only the device knows which zone it is
// in. Refusing the hint does not make us safer, it makes us wrong.
//
// What the hint may NOT do is override a choice the user made. Hence two
// fields rather than one:
//
//	Timezone        the value
//	TimezoneSource  "user" (settings page) or "detected" (a client hint)
//
// A hint fills in or updates a detected value; it never overwrites a user's own.
// Otherwise a user who deliberately keeps their schedule on home time would have
// it silently moved the first time they opened the app from an airport.
//
// The same source-tagging pattern is already used by MoodCheckin.Source and
// TimeBlock.LockSource, for the same reason: "who decided this" is a different
// question from "what is it".

// Timezone sources.
const (
	TZSourceUser     = "user"
	TZSourceDetected = "detected"
)

// sessionLocation resolves the location to use for a session's own clock.
//
// Falls back to the deployment default and then to UTC. It never returns nil:
// every caller is doing date arithmetic and a nil location would panic at the
// worst possible moment, deep inside a background job.
func (s *Server) sessionLocation(ctx context.Context, sid string) *time.Location {
	return resolveLocation(s.sessionTimezone(ctx, sid))
}

// SessionTimezone is sessionTimezone for callers outside the package (main.go
// schedules a session's cron entries on first use).
func (s *Server) SessionTimezone(ctx context.Context, sid string) string {
	return s.sessionTimezone(ctx, sid)
}

// sessionTimezone returns the IANA name, deployment default when the session has
// none.
func (s *Server) sessionTimezone(ctx context.Context, sid string) string {
	if s == nil || s.store == nil || sid == "" {
		return s.defaultTimezone()
	}
	if tz := s.sessionPrefs(ctx, sid).Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	return s.defaultTimezone()
}

func (s *Server) defaultTimezone() string {
	if s == nil || s.cfg == nil || s.runtime().WorkerDefaultTZ == "" {
		return "UTC"
	}
	return s.runtime().WorkerDefaultTZ
}

// validTimezone reports whether a client-supplied string is an IANA zone this
// build can load.
//
// ⚠️ The screen below is a BOUND, not the guard. time.LoadLocation already
// refuses traversal, NUL bytes and over-long names — it is the thing that
// actually decides. What the screen buys is that an unbounded string from a
// request body does not reach a filesystem-ish lookup at all, which is cheap
// insurance against a future tzdata implementation being less careful than this
// one. Do not delete it, and do not believe it is what makes this safe.
func validTimezone(tz string) bool {
	tz = strings.TrimSpace(tz)
	if tz == "" || len(tz) > 64 ||
		strings.ContainsAny(tz, "\x00\\") || strings.Contains(tz, "..") || strings.HasPrefix(tz, "/") {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// noteClientTimezone records a timezone a client reported, if we do not already
// have a better answer.
//
// Best-effort and asynchronous-ish in spirit: this is called from request paths
// that have their own job to do, and a session whose timezone could not be saved
// is exactly as broken as it was a moment ago. It must never fail a request.
//
// Rescheduling on change is the point of the whole exercise — a stored timezone
// that the cron entries do not reflect is a value nobody reads.
func (s *Server) noteClientTimezone(ctx context.Context, sid, tz string) {
	if s == nil || s.store == nil || sid == "" || !validTimezone(tz) {
		return
	}
	// validTimezone trims for its screen but the caller would store the raw
	// string — " Asia/Shanghai " passes validation and then fails every
	// LoadLocation forever, an accepted hint that never takes effect. The
	// stored value must be the value that was validated.
	tz = strings.TrimSpace(tz)
	prefs := s.sessionPrefs(ctx, sid)
	if prefs.Timezone == tz {
		return
	}
	if prefs.TimezoneSource == TZSourceUser {
		// The user chose this on the settings page. A device hint does not get to
		// overrule it — see the file comment.
		return
	}
	prefs.Timezone = tz
	prefs.TimezoneSource = TZSourceDetected
	s.saveSessionTimezone(ctx, sid, prefs)
}

// aiClockTimezone resolves the zone an AI handler must build its clock from: the
// session's own value, with the client's device hint folded in first.
//
// ⚠️ ONE entry point rather than "call noteClientTimezone, then remember to read
// sessionTimezone". The defect this replaces was exactly a handler that recorded
// the hint and then handed the RAW request field to the prompt builders, so a
// client that sent no zone got time.LoadLocation("") and the fallback in
// companionSystemPrompt was time.UTC: a reader in Chicago at 23:44 was told it was
// 04:44 — the next day, in a zone nobody in that conversation was in. It also
// silently overrode a settings-page choice, so someone who keeps their schedule on
// home time lost it whenever their device reported another zone.
//
// Both failure modes are the same mistake — treating "what the client sent" as
// the answer instead of a hint — so they are closed in one place.
func (s *Server) aiClockTimezone(ctx context.Context, sid, clientHint string) string {
	s.noteClientTimezone(ctx, sid, clientHint)
	return s.sessionTimezone(ctx, sid)
}

// saveSessionTimezone persists prefs and re-arms the session's cron entries.
func (s *Server) saveSessionTimezone(ctx context.Context, sid string, prefs SessionPrefs) {
	raw, err := json.Marshal(prefs)
	if err != nil {
		return
	}
	str := string(raw)
	if _, err := s.store.Sessions().Update(ctx, sid, domain.SessionUpdate{Preferences: &str}); err != nil {
		s.log.Debug("could not store the session timezone", "sid", sid, "err", err)
		return
	}
	s.log.Info("session timezone updated", "sid", sid, "tz", prefs.Timezone, "source", prefs.TimezoneSource)
	// The cron entries were built with CRON_TZ of the old zone. ScheduleUser
	// removes the old ones before adding new — which is why it had to learn to
	// do that before this could ship.
	if s.worker != nil {
		s.worker.ScheduleUser(sid, prefs.Timezone)
	}
}
