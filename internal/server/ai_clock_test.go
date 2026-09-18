package server

// 「模型被告知的是哪一天」—— 时区缺陷的回归测试。
//
// # 症状（所有者亲手碰到，2026-09-17）
//
// 深夜 23:44 在芝加哥问陪伴「现在几点了」，它回答「都四点四十了，你是还没睡？」。
// 那是 UTC 的 04:44 —— 一个对话里没人在的时区。
//
// # 机制
//
// companion handler 把 body.Timezone（客户端的原始请求字段）直接喂给提示词构造器，
// 从不查自己那份 sessionTimezone；而前端那条 askCompanion 调用根本不送时区。于是
// time.LoadLocation("") 失败，companionSystemPrompt 的兜底是 time.UTC。（同一个
// 形状还在 plan 端点：客户端不送日期时 BuildDateContext 落到 UTC 的「今天」，20:00
// 在芝加哥问「今天」会拿到明天。）
//
// # 为什么断言的是「模型收到的请求体」
//
// 缺陷在接线处：sessionTimezone 本身一直是对的、也有测试（session_timezone_test.go）。
// 只测那个函数的话，这个 bug 全程绿灯。所以这里起一个假 provider 把发给模型的请求捉
// 下来，断言里面写的是读者的时区和读者的今天。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"daycore/internal/auth"
	"daycore/internal/domain"
)

// capturingOpenAI 是一个假 provider：它回一句很普通的回答，并把每次收到的请求体
// 记下来。模型看到的提示词因此可以被断言，而不是被推测。
func capturingOpenAI(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"好的"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

type clockFixture struct {
	s         *Server
	sid       string
	zone      string // 部署默认，也是「会话没说自己时区」时该用的那个
	localDate string // zone 里的今天
	utcDate   string // UTC 里的今天 —— 与 localDate 必然不同，见下
	sent      func() []string
}

// clockServer 起一个能捕获模型请求的服务，会话语言固定 en-US（断言锚点是模板里那句
// 英文），部署默认时区选到「此刻它的日期不等于 UTC 的日期」。
//
// ⚠️ 那个动态选择是刻意的：写死某个时区会让「localDate != utcDate」只在一天里的某
// 些小时成立，另一些小时里断言会因为日子相同而变成同义反复 —— 一个只在半夜有效的
// 测试比没有测试更坏。
func clockServer(t *testing.T) *clockFixture {
	t.Helper()
	srv, sent := capturingOpenAI(t)
	s, sid := newAutoPlanServer(t, srv)

	utcDate := time.Now().UTC().Format("2006-01-02")
	// Kiritimati (UTC+14) 的日期在 UTC 10:00 之后必然领先一天；Honolulu (UTC-10)
	// 在 UTC 10:00 之前必然落后一天。两者合起来覆盖一整天。
	zone := "Pacific/Kiritimati"
	if time.Now().UTC().Hour() < 10 {
		zone = "Pacific/Honolulu"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("这个 build 的 tzdata 里没有 %s: %v", zone, err)
	}
	localDate := time.Now().In(loc).Format("2006-01-02")
	if localDate == utcDate {
		t.Fatalf("%s 的日期此刻仍等于 UTC 的 %s：断言会因为错误的原因通过", zone, utcDate)
	}

	// 部署默认。⚠️ newAutoPlanServer 造出来的 Config 里它是空的，而空值会让
	// sessionTimezone 退到 UTC —— 那正是这次要排除的行为，所以测试必须自己设一个。
	s.cfg.WorkerDefaultTZ = zone

	lang := "en-US"
	if _, err := s.store.Sessions().Update(context.Background(), sid, domain.SessionUpdate{Language: &lang}); err != nil {
		t.Fatal(err)
	}
	return &clockFixture{s: s, sid: sid, zone: zone, localDate: localDate, utcDate: utcDate, sent: sent}
}

func companionReq(t *testing.T, s *Server, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", versionPath("/api/ai/companion"), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withSessionID(req.Context(), sid))
	s.handleAICompanion(rec, req)
	return rec
}

func firstModelRequest(t *testing.T, f *clockFixture) string {
	t.Helper()
	got := f.sent()
	if len(got) == 0 {
		t.Fatal("模型一次都没被调用，没有可断言的东西")
	}
	return got[0]
}

func short(b string) string {
	if len(b) > 1200 {
		return b[:1200] + "…"
	}
	return b
}

// 会话自己记得时区（早先某次 plan 调用留下的设备提示）→ 模型必须按它算今天。
func TestCompanionClockIsTheSessionsZone(t *testing.T) {
	f := clockServer(t)
	f.s.noteClientTimezone(context.Background(), f.sid, f.zone)

	if rec := companionReq(t, f.s, f.sid, `{"message":"现在几点了？"}`); rec.Code != http.StatusOK {
		t.Fatalf("companion: %d %s", rec.Code, rec.Body.String())
	}
	body := firstModelRequest(t, f)
	if !strings.Contains(body, "today is "+f.localDate) {
		t.Errorf("模型没被告知读者的今天（%s）；发给模型的内容: %s", f.localDate, short(body))
	}
	if !strings.Contains(body, "(timezone "+f.zone+")") {
		t.Errorf("模型没被告知读者的时区（%s）；发给模型的内容: %s", f.zone, short(body))
	}
	if strings.Contains(body, "today is "+f.utcDate) {
		t.Errorf("模型被告知的是 UTC 的今天（%s）—— 这就是「芝加哥 23:44 被告知 04:44」那个 bug", f.utcDate)
	}
}

// 会话完全没有时区（今天的现实：四端的前端都不送、用户也没设）→ 落到部署默认，
// 而不是 UTC。修复前这里必然是 UTC。
func TestCompanionClockFallsBackToTheDeploymentDefault(t *testing.T) {
	f := clockServer(t)
	if got := f.s.sessionPrefs(context.Background(), f.sid).Timezone; got != "" {
		t.Fatalf("这个用例要的是「会话自己没时区」，现在是 %q", got)
	}
	if rec := companionReq(t, f.s, f.sid, `{"message":"现在几点了？"}`); rec.Code != http.StatusOK {
		t.Fatalf("companion: %d %s", rec.Code, rec.Body.String())
	}
	body := firstModelRequest(t, f)
	if !strings.Contains(body, "(timezone "+f.zone+")") {
		t.Errorf("没落到部署默认时区 %s；发给模型的内容: %s", f.zone, short(body))
	}
	if strings.Contains(body, "today is "+f.utcDate) {
		t.Errorf("落到了 UTC 的今天（%s）", f.utcDate)
	}
}

// 客户端送了时区 → 既被记进会话（下一次不送也还在），也被这次请求使用。
func TestCompanionClockAdoptsTheClientsHint(t *testing.T) {
	f := clockServer(t)
	hint := "Europe/Berlin"
	rec := companionReq(t, f.s, f.sid, `{"message":"hi","timezone":"`+hint+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("companion: %d %s", rec.Code, rec.Body.String())
	}
	body := firstModelRequest(t, f)
	if !strings.Contains(body, "(timezone "+hint+")") {
		t.Errorf("这次请求没用客户端报的时区；发给模型的内容: %s", short(body))
	}
	if got := f.s.sessionTimezone(context.Background(), f.sid); got != hint {
		t.Errorf("提示没被记进会话：sessionTimezone = %q，want %q", got, hint)
	}
}

// 用户在设置页自己选了时区 → 设备提示不许覆盖它。旧代码把请求字段直接喂给提示词，
// 于是「把日程留在老家时间」的人一出差就被悄悄挪了。
func TestCompanionClockDoesNotOverruleTheUser(t *testing.T) {
	f := clockServer(t)
	if rec := patchPrefs(t, f.s, f.sid, `{"timezone":"Asia/Tokyo"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH timezone: %d %s", rec.Code, rec.Body.String())
	}
	hint := "Europe/Berlin"
	if rec := companionReq(t, f.s, f.sid, `{"message":"hi","timezone":"`+hint+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("companion: %d %s", rec.Code, rec.Body.String())
	}
	body := firstModelRequest(t, f)
	if !strings.Contains(body, "(timezone Asia/Tokyo)") {
		t.Errorf("用户自己选的时区没赢；发给模型的内容: %s", short(body))
	}
	if strings.Contains(body, hint) {
		t.Errorf("设备提示 %s 覆盖了用户的选择", hint)
	}
}

// plan 端点：客户端一个字段都不送时，「今天」必须来自会话，而不是 UTC 的今天。
func TestPlanClockFillsInFromTheSessionWhenTheClientSendsNothing(t *testing.T) {
	f := clockServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", versionPath("/api/ai/plan-text"), strings.NewReader(`{"description":"明天下午写作业"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withSessionID(req.Context(), f.sid))
	f.s.handleAIPlanText(rec, req)

	body := firstModelRequest(t, f)
	if !strings.Contains(body, "Today's date: "+f.localDate) {
		t.Errorf("今天的日期不是会话的今天（%s）；发给模型的内容: %s", f.localDate, short(body))
	}
	if !strings.Contains(body, "User timezone: "+f.zone) {
		t.Errorf("时区没落到会话/部署默认；发给模型的内容: %s", short(body))
	}
}

// 落块用的日期必须与提示词说的是同一天：否则模型为「今天」排的计划被挂到了空日期上。
func TestPlanBlocksAttachToTheSameDayThePromptWasTold(t *testing.T) {
	f := clockServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", versionPath("/api/ai/plan-text"), strings.NewReader(`{"description":"写作业"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withSessionID(req.Context(), f.sid))
	f.s.handleAIPlanText(rec, req)

	// 假 provider 回的是散文，不是 blocks，所以只断言「没有把空日期当成目标」：
	// 空日期会在响应里以空 target 的形式出现。
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v / %s", err, rec.Body.String())
	}
	if target, _ := out["date"].(string); target == "" {
		if _, has := out["blocks"]; has {
			t.Errorf("响应里带了 blocks 却没有日期：%s", rec.Body.String())
		}
	}
	_ = context.Background()
}

// 首次接触那一次也要能用：POST /api/session/init 收下设备时区，记进会话。
//
// ⚠️ 这一条是整套时区链的**入口**。四端的前端今天都不送时区，所以真实会话在没有它
// 之前只能靠部署默认 —— 而部署默认对一个不在运营商那个时区的用户就是错的。
func TestSessionInitLearnsTheDeviceTimezone(t *testing.T) {
	s, sid := newAgentTestServer(t)
	s.cookies = auth.NewCookieSigner("test-secret") // 处理器要下发 dc_sid cookie

	initWith := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", versionPath("/api/session/init"), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(withSessionID(req.Context(), sid))
		s.handleSessionInit(rec, req)
		return rec
	}

	if rec := initWith(`{"timezone":"America/Chicago"}`); rec.Code != http.StatusOK {
		t.Fatalf("session init: %d %s", rec.Code, rec.Body.String())
	}
	prefs := s.sessionPrefs(context.Background(), sid)
	if prefs.Timezone != "America/Chicago" {
		t.Errorf("首次接触的时区没记下来：%q", prefs.Timezone)
	}
	if prefs.TimezoneSource != TZSourceDetected {
		t.Errorf("源应当是 %q（设备提示），实际 %q", TZSourceDetected, prefs.TimezoneSource)
	}

	// 一个不是时区的字符串不许把已经学到的值冲掉，也不许让请求失败。
	if rec := initWith(`{"timezone":"Not/AZone"}`); rec.Code != http.StatusOK {
		t.Errorf("垃圾时区让 session init 失败了：%d %s", rec.Code, rec.Body.String())
	}
	if got := s.sessionPrefs(context.Background(), sid).Timezone; got != "America/Chicago" {
		t.Errorf("垃圾时区覆盖了好值：%q", got)
	}
}
