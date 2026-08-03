package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func TestRecorderWritesDailyFile(t *testing.T) {
	dir := t.TempDir()
	inner := &spySink{}
	r := NewRecorder(inner, dir, "desktop")

	r.Emit(usageEvent("deepseek/deepseek-v4-flash", 100, 50, 10, 20, 30, 150))
	r.Emit(usageEvent("deepseek/deepseek-v4-pro", 200, 100, 0, 0, 0, 300))
	r.Emit(turnEvent())

	// The daily file must exist with three lines (2 usage + 1 turn marker).
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 daily file, got %d", len(files))
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 3 {
		t.Fatalf("want 3 lines, got %d", lines)
	}
	// Forwarding must be untouched.
	if len(inner.events) != 3 {
		t.Fatalf("want 3 forwarded events, got %d", len(inner.events))
	}
}

func TestRecorderSkipsZeroUsage(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(&spySink{}, dir, "desktop")
	r.Emit(usageEvent("m", 0, 0, 0, 0, 0, 0)) // TotalTokens <= 0 -> skipped
	r.Emit(turnEvent())
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file (turn only), got %d", len(files))
	}
}

func TestRecorderDisabledOnEmptyDir(t *testing.T) {
	r := NewRecorder(&spySink{}, "", "desktop")
	r.Emit(usageEvent("m", 1, 1, 0, 0, 0, 2))
	r.Emit(turnEvent())
	// No panic, nothing written — query on empty dir returns zeros.
	got, err := r.writer.Query(SourceFilter{From: time.Now().Add(-24 * time.Hour), To: time.Now()})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Tokens != 0 || got.Turns != 0 {
		t.Fatalf("want zero stats, got %+v", got)
	}
}

func TestQueryAggregates(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	now := time.Now()
	day := dayStart(now)

	// Two usage rows + one turn on "today", one usage row yesterday.
	w.Append(record{Timestamp: day.Add(1 * time.Hour), ModelRef: "deepseek/deepseek-v4-flash", Source: "desktop", Total: 100, Prompt: 60, Completion: 40, CacheHit: 10, CacheMiss: 50})
	w.Append(record{Timestamp: day.Add(2 * time.Hour), ModelRef: "deepseek/deepseek-v4-pro", Source: "desktop", Total: 200, Prompt: 100, Completion: 100})
	w.Append(record{Timestamp: day.Add(3 * time.Hour), Source: "desktop", Turn: true})
	w.Append(record{Timestamp: day.AddDate(0, 0, -1), ModelRef: "zhipu/glm-5.2", Source: "cli", Total: 300})

	got, err := w.Query(SourceFilter{From: day.AddDate(0, 0, -1), To: day})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Tokens != 600 {
		t.Fatalf("tokens: want 600, got %d", got.Tokens)
	}
	if got.Requests != 3 {
		t.Fatalf("requests: want 3, got %d", got.Requests)
	}
	if got.Turns != 1 {
		t.Fatalf("turns: want 1, got %d", got.Turns)
	}
	if got.CacheHit != 10 || got.CacheMiss != 50 {
		t.Fatalf("cache: want hit=10 miss=50, got hit=%d miss=%d", got.CacheHit, got.CacheMiss)
	}
	if got.ActiveDays != 2 {
		t.Fatalf("active days: want 2, got %d", got.ActiveDays)
	}
	if got.TopModel != "zhipu/glm-5.2" {
		t.Fatalf("top model: want zhipu/glm-5.2 (300 tokens), got %q", got.TopModel)
	}
	if len(got.Daily) != 2 {
		t.Fatalf("daily series: want 2 entries, got %d", len(got.Daily))
	}
	// daysInRange walks from -> to, so Daily[0] is yesterday (glm, no cache)
	// and Daily[1] is today (flash hit=10 miss=50 + pro no cache).
	if got.Daily[0].CacheHit != 0 || got.Daily[0].CacheMiss != 0 {
		t.Fatalf("yesterday cache: want 0/0, got hit=%d miss=%d", got.Daily[0].CacheHit, got.Daily[0].CacheMiss)
	}
	if got.Daily[1].CacheHit != 10 || got.Daily[1].CacheMiss != 50 {
		t.Fatalf("today cache: want hit=10 miss=50, got hit=%d miss=%d", got.Daily[1].CacheHit, got.Daily[1].CacheMiss)
	}
	if len(got.Models) != 3 {
		t.Fatalf("models: want 3, got %d", len(got.Models))
	}
	// Providers: deepseek (100+200=300), zhipu (300) — tied, so find by name.
	found := map[string]int64{}
	for _, p := range got.Providers {
		found[p.Provider] = p.Tokens
	}
	if found["deepseek"] != 300 || found["zhipu"] != 300 || len(found) != 2 {
		t.Fatalf("providers: want deepseek=300 zhipu=300, got %+v", got.Providers)
	}
	// Percent on models sums to ~100 across 3 models: 200/600=33.3, 100/600=16.7, 300/600=50
	if got.Models[0].Percent <= 0 || got.Models[0].Percent > 100 {
		t.Fatalf("model percent out of range: %+v", got.Models[0])
	}
}

func TestQuerySourceFilter(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	now := time.Now()
	day := dayStart(now)

	w.Append(record{Timestamp: day, ModelRef: "m1", Source: "desktop", Total: 100})
	w.Append(record{Timestamp: day, ModelRef: "m2", Source: "cli", Total: 50})

	got, err := w.Query(SourceFilter{From: day, To: day, Source: "cli"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Tokens != 50 {
		t.Fatalf("cli-filtered tokens: want 50, got %d", got.Tokens)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "m2" {
		t.Fatalf("cli-filtered models: want [m2], got %+v", got.Models)
	}
}

func TestQueryEmptyRange(t *testing.T) {
	w := NewWriter(t.TempDir())
	now := time.Now()
	got, err := w.Query(SourceFilter{From: now, To: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Tokens != 0 || got.ActiveDays != 0 || len(got.Daily) != 0 {
		t.Fatalf("want empty stats, got %+v", got)
	}
}

func TestDecodeRecordsSkipsMalformed(t *testing.T) {
	// A torn or hand-edited line must not fail the whole day's read: it is
	// skipped and the surrounding valid records still come through.
	good := `{"ts":"2026-08-02T10:00:00+08:00","total":100}` + "\n"
	bad := `{"ts":"2026-08-02T10:00:00+08:00","total":` + "\n" // truncated JSON
	recs, err := decodeRecords(strings.NewReader(good + bad + bad + good))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 valid records, got %d", len(recs))
	}
	for _, r := range recs {
		if r.Total != 100 {
			t.Fatalf("record total: want 100, got %d", r.Total)
		}
	}
}

// TestDailyTokensWireKeys guards the JSON contract the desktop panel reads:
// the hand-written frontend types use camelCase (byModel/byProvider), so a
// snake_case tag here silently yields undefined fields in DailyTrend and
// crashed the panel with "Cannot convert undefined or null to object".
func TestDailyTokensWireKeys(t *testing.T) {
	d := DailyTokens{Day: "2026-08-02", Total: 150, ByModel: map[string]int64{"deepseek/x": 150}, Requests: 2, Turns: 1, CacheHit: 10, CacheMiss: 50}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"day", "total", "byModel", "byProvider", "requests", "turns", "cacheHit", "cacheMiss"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("wire key %q missing from %s", want, b)
		}
	}
	for _, bad := range []string{"by_model", "by_provider", "cache_hit", "cache_miss"} {
		if _, ok := keys[bad]; ok {
			t.Fatalf("legacy snake_case key %q still present in %s", bad, b)
		}
	}
}

func TestProviderSplit(t *testing.T) {
	if got := providerOf("deepseek/deepseek-v4-flash"); got != "deepseek" {
		t.Fatalf("provider: want deepseek, got %q", got)
	}
	if got := providerOf("bare-model"); got != "default" {
		t.Fatalf("bare model: want default, got %q", got)
	}
}

// --- test helpers ---

type spySink struct{ events []event.Event }

func (s *spySink) Emit(e event.Event) { s.events = append(s.events, e) }

func usageEvent(model string, prompt, completion, reasoning, hit, miss, total int) event.Event {
	return event.Event{
		Kind:     event.Usage,
		ModelRef: model,
		Usage: &provider.Usage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			ReasoningTokens:  reasoning,
			CacheHitTokens:   hit,
			CacheMissTokens:  miss,
			TotalTokens:      total,
		},
	}
}

func turnEvent() event.Event { return event.Event{Kind: event.TurnDone} }
