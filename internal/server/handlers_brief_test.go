package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"daycore/internal/config"
	"daycore/internal/storage"
)

// A session with no plan row yet must get the gentle empty state, not a 500.
//
// ⚠️ This is the FIRST state of every account on a fresh deployment, not an
// edge case. The first version tolerated domain.ErrNotFound and then read
// plan.Blocks off the nil plan, so the empty branch below was unreachable and
// the endpoint 500'd precisely where it was supposed to be at its gentlest.
func TestBriefOnSessionWithNoPlanIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open("sqlite", "file:"+t.TempDir()+"/brief.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sid := "brief-empty-sid"
	if _, err := store.Sessions().GetOrCreate(ctx, sid); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{
		Config: &config.Config{RateLimitPerMin: 0},
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(new(strings.Builder), nil)),
	})
	// Registered AFTER the store's Close so it runs BEFORE it (cleanups are LIFO).
	t.Cleanup(func() {
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.WaitBackground(wctx); err != nil {
			t.Errorf("background work outlived the test by more than 5s: %v", err)
		}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, versionPath("/api/brief"), nil).
		WithContext(withSessionID(ctx, sid))
	s.handleBrief(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET brief on a plan-less session = %d, want 200 (body: %s)\n"+
			"An empty day is the normal first state of an account; it is not an error.",
			rec.Code, rec.Body.String())
	}
	var got struct {
		Empty bool     `json:"empty"`
		Date  string   `json:"date"`
		Title string   `json:"title"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, rec.Body.String())
	}
	if !got.Empty {
		t.Errorf("empty = false for a session with no plan")
	}
	if got.Title == "" || len(got.Lines) == 0 {
		t.Errorf("empty state came back without its gentle copy: %+v", got)
	}
}
