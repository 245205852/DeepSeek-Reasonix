package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// trajectorySummary is the harness-side digest of one run's trajectory file:
// where the wall clock went, split between tool execution and everything the
// model spent between calls (thinking, streaming, provider latency).
type trajectorySummary struct {
	Path    string `json:"path"`
	Records int    `json:"records"`
	SpanMs  int64  `json:"span_ms"`
	ToolMs  int64  `json:"tool_ms"`
	ModelMs int64  `json:"model_ms"` // SpanMs − tool wall clock, floored at zero
	// Round decomposition: a round's gap runs from the turn start (or the
	// batch's last tool_result) to the next top-level tool_dispatch; the
	// final segment to the last record is the answer round.
	ModelRounds     int   `json:"model_rounds"`
	ModelGapTotalMs int64 `json:"model_gap_total_ms"`
	ModelGapP95Ms   int64 `json:"model_gap_p95_ms"`
	Retries         int   `json:"retries,omitempty"`
	Compactions     int   `json:"compactions,omitempty"`

	// Batch decomposition: one batch is one round's top-level tool calls.
	// ToolWallMs is the union of execution intervals (parallel calls counted
	// once) plus durations of calls that carried no timestamps.
	ToolWallMs       int64 `json:"tool_wall_ms,omitempty"`
	ToolBatches      int   `json:"tool_batches,omitempty"`
	TopLevelCalls    int   `json:"top_level_calls,omitempty"`
	MaxBatchSize     int   `json:"max_batch_size,omitempty"`
	ParallelBatches  int   `json:"parallel_batches,omitempty"`   // ≥2 calls actually overlapped
	ParallelSavedMs  int64 `json:"parallel_saved_ms,omitempty"`  // Σ durations − batch wall
	SingleReadRounds int   `json:"single_read_rounds,omitempty"` // 1-call read-only batches
	SingleReadStreak int   `json:"single_read_streak,omitempty"` // longest consecutive run
	StartDelayP95Ms  int64 `json:"start_delay_p95_ms,omitempty"` // dispatch→start, in-batch queue

	// Recovery decomposition: a round whose gap contained a provider retry,
	// missing-reasoning replay, or empty-final retry is a recovery round; clean
	// p95 excludes them so adapter flakiness reads as adapter cost, not agent.
	StreamRetries     int   `json:"stream_retries,omitempty"`
	HeaderRetries     int   `json:"header_retries,omitempty"`
	ReasoningReplays  int   `json:"reasoning_replays,omitempty"` // extra exact-replay requests
	EmptyFinalRetries int   `json:"empty_final_retries,omitempty"`
	RecoveryRounds    int   `json:"recovery_rounds,omitempty"`
	RecoveryGapMs     int64 `json:"recovery_gap_ms,omitempty"`
	CleanGapP95Ms     int64 `json:"clean_gap_p95_ms,omitempty"`
}

// toolWall is the best available tool wall-clock: interval union when the
// recording carried timestamps, else the duration sum (older trajectories).
func (s *trajectorySummary) toolWall() int64 {
	if s.ToolWallMs > 0 {
		return s.ToolWallMs
	}
	return s.ToolMs
}

// trajectoryRecord is the subset of trajectory.Record the summary needs.
type trajectoryRecord struct {
	TS               int64  `json:"ts"`
	ProtocolRecovery string `json:"protocol_recovery"`
	Event            *struct {
		Kind       string `json:"kind"`
		Code       string `json:"code"`
		RetryScope string `json:"retryScope"`
		Tool       *struct {
			ID         string `json:"id"`
			DurationMs int64  `json:"durationMs"`
			ParentID   string `json:"parentId"`
			ReadOnly   bool   `json:"readOnly"`
			Refreshed  bool   `json:"refreshed"`
			StartedAt  int64  `json:"startedAt"`
			EndedAt    int64  `json:"endedAt"`
		} `json:"tool"`
	} `json:"event"`
}

// toolBatch accumulates one round's top-level calls between model gaps.
type toolBatch struct {
	dispatchTS map[string]int64
	calls      int
	results    int
	readOnly   int
	serialMs   int64
	intervals  [][2]int64
}

// renderTimeAttribution aggregates recorded runs into one report line; empty
// when no run in the suite carried a trajectory.
func renderTimeAttribution(results []result) string {
	var toolMs, modelMs, gapMs, savedMs, delayP95, recoveryGapMs, cleanP95 int64
	runs, rounds, batches, calls, singleReads, parallelBatches := 0, 0, 0, 0, 0, 0
	recoveryRounds, streamRetries, headerRetries, replays, emptyFinals := 0, 0, 0, 0, 0
	for _, r := range results {
		if r.Trajectory != nil {
			runs++
			toolMs += r.Trajectory.toolWall()
			modelMs += r.Trajectory.ModelMs
			rounds += r.Trajectory.ModelRounds
			gapMs += r.Trajectory.ModelGapTotalMs
			batches += r.Trajectory.ToolBatches
			calls += r.Trajectory.TopLevelCalls
			singleReads += r.Trajectory.SingleReadRounds
			parallelBatches += r.Trajectory.ParallelBatches
			savedMs += r.Trajectory.ParallelSavedMs
			delayP95 = max(delayP95, r.Trajectory.StartDelayP95Ms)
			recoveryRounds += r.Trajectory.RecoveryRounds
			recoveryGapMs += r.Trajectory.RecoveryGapMs
			cleanP95 = max(cleanP95, r.Trajectory.CleanGapP95Ms)
			streamRetries += r.Trajectory.StreamRetries
			headerRetries += r.Trajectory.HeaderRetries
			replays += r.Trajectory.ReasoningReplays
			emptyFinals += r.Trajectory.EmptyFinalRetries
		}
	}
	if runs == 0 {
		return ""
	}
	line := fmt.Sprintf("**Time attribution** (%d recorded runs): **tools** %s (%s) · **model** %s (%s)",
		runs, dur(toolMs), pct(int(toolMs), int(toolMs+modelMs)),
		dur(modelMs), pct(int(modelMs), int(toolMs+modelMs)))
	if rounds > 0 {
		line += fmt.Sprintf(" · **model rounds** %d (avg gap %s)", rounds, dur(gapMs/int64(rounds)))
	}
	if batches > 0 {
		line += fmt.Sprintf("\n\n**Batching** (%d tool rounds): **calls/round** %.1f · **single-read rounds** %d (%s) · **parallel rounds** %d (saved %s) · **start-delay p95** %s",
			batches, float64(calls)/float64(batches),
			singleReads, pct(singleReads, batches),
			parallelBatches, dur(savedMs), durMs(delayP95))
	}
	if recoveryRounds+streamRetries+headerRetries+replays+emptyFinals > 0 {
		line += fmt.Sprintf("\n\n**Recovery**: recovery rounds %d (%s of rounds, %s) · stream retries %d · header retries %d · reasoning replays %d · empty-final retries %d · clean gap p95 %s",
			recoveryRounds, pct(recoveryRounds, rounds), dur(recoveryGapMs),
			streamRetries, headerRetries, replays, emptyFinals, durMs(cleanP95))
	}
	return line + "\n\n"
}

// trajScan is the running state of one trajectory pass.
type trajScan struct {
	s                  *trajectorySummary
	firstTS, lastTS    int64
	orphanMs, gapStart int64
	gaps, cleanGaps    []int64
	delays             []int64
	allIntervals       [][2]int64
	inModel            bool
	tainted            bool
	streakRun          int
	batch              *toolBatch
}

// summarizeTrajectory reads a run's JSONL trajectory. A truncated final line
// (killed run) is skipped, matching the recorder's durability contract.
func summarizeTrajectory(path string) (*trajectorySummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scan := &trajScan{s: &trajectorySummary{Path: path}, batch: newToolBatch()}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec trajectoryRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		scan.record(rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return scan.finish(), nil
}

func (t *trajScan) record(rec trajectoryRecord) {
	t.s.Records++
	if t.firstTS == 0 {
		t.firstTS = rec.TS
		t.gapStart = rec.TS
		t.inModel = true
	}
	t.lastTS = rec.TS
	if rec.ProtocolRecovery == "missing_reasoning_retry_attempted" {
		t.s.ReasoningReplays++
		t.tainted = true
	}
	if rec.Event == nil {
		return
	}
	switch rec.Event.Kind {
	case "retrying":
		t.s.Retries++
		t.tainted = true
		switch rec.Event.RetryScope {
		case "stream":
			t.s.StreamRetries++
		case "headers":
			t.s.HeaderRetries++
		}
	case "notice":
		if rec.Event.Code == "empty_final" {
			t.s.EmptyFinalRetries++
			t.tainted = true
		}
	case "compaction_started":
		t.s.Compactions++
	}
	if rec.Event.Tool == nil || rec.Event.Tool.ParentID != "" {
		// Subagent calls overlap the parent's wall clock; counting them
		// would double-book the span and split the parent's rounds.
		return
	}
	switch rec.Event.Kind {
	case "tool_dispatch":
		t.recordDispatch(rec)
	case "tool_result":
		t.recordResult(rec)
	}
}

// closeGap ends one model round; recovery-tainted rounds are booked apart so
// clean latency stays comparable across providers with different flake rates.
func (t *trajScan) closeGap(gap int64) {
	t.gaps = append(t.gaps, gap)
	if t.tainted {
		t.s.RecoveryRounds++
		t.s.RecoveryGapMs += gap
		t.tainted = false
		return
	}
	t.cleanGaps = append(t.cleanGaps, gap)
}

func (t *trajScan) recordDispatch(rec trajectoryRecord) {
	tl := rec.Event.Tool
	if t.inModel {
		t.closeGap(rec.TS - t.gapStart)
		t.inModel = false
		t.closeBatch()
	}
	// Refreshed dispatches re-announce a call already counted;
	// id-less records (older recordings) cannot be deduped.
	if tl.Refreshed {
		return
	}
	if tl.ID == "" || !t.batch.seen(tl.ID) {
		t.batch.calls++
	}
	// The full dispatch re-announces a streamed partial; keeping the later TS
	// anchors start-delay to pre-exec queueing, not the stream tail.
	if tl.ID != "" {
		t.batch.dispatchTS[tl.ID] = rec.TS
	}
}

func (t *trajScan) recordResult(rec trajectoryRecord) {
	tl := rec.Event.Tool
	t.s.ToolMs += tl.DurationMs
	t.batch.results++
	if tl.ReadOnly {
		t.batch.readOnly++
	}
	t.batch.serialMs += tl.DurationMs
	if tl.StartedAt > 0 && tl.EndedAt >= tl.StartedAt {
		t.batch.intervals = append(t.batch.intervals, [2]int64{tl.StartedAt, tl.EndedAt})
		if disp, ok := t.batch.dispatchTS[tl.ID]; ok && tl.StartedAt >= disp {
			t.delays = append(t.delays, tl.StartedAt-disp)
		}
	} else {
		t.orphanMs += tl.DurationMs
	}
	t.gapStart = rec.TS
	t.inModel = true
}

func (t *trajScan) closeBatch() {
	b, s := t.batch, t.s
	if b.calls == 0 {
		return
	}
	s.ToolBatches++
	s.TopLevelCalls += b.calls
	s.MaxBatchSize = max(s.MaxBatchSize, b.calls)
	if b.calls == 1 && b.results == 1 && b.readOnly == 1 {
		s.SingleReadRounds++
		t.streakRun++
		s.SingleReadStreak = max(s.SingleReadStreak, t.streakRun)
	} else {
		t.streakRun = 0
	}
	if len(b.intervals) > 1 {
		wall, overlapped := intervalSpan(b.intervals)
		if overlapped {
			s.ParallelBatches++
		}
		if saved := b.serialMs - wall; saved > 0 {
			s.ParallelSavedMs += saved
		}
	}
	t.allIntervals = append(t.allIntervals, b.intervals...)
	t.batch = newToolBatch()
}

func (t *trajScan) finish() *trajectorySummary {
	s := t.s
	if t.inModel && t.lastTS > t.gapStart {
		t.closeGap(t.lastTS - t.gapStart) // final answer round
	}
	t.closeBatch()
	s.ModelRounds = len(t.gaps)
	for _, g := range t.gaps {
		s.ModelGapTotalMs += g
	}
	s.ModelGapP95Ms = p95(t.gaps)
	s.CleanGapP95Ms = p95(t.cleanGaps)
	s.StartDelayP95Ms = p95(t.delays)
	if len(t.allIntervals) > 0 {
		s.ToolWallMs = intervalUnion(t.allIntervals) + t.orphanMs
	}
	s.SpanMs = t.lastTS - t.firstTS
	if s.ModelMs = s.SpanMs - s.toolWall(); s.ModelMs < 0 {
		s.ModelMs = 0
	}
	return s
}

func newToolBatch() *toolBatch {
	return &toolBatch{dispatchTS: map[string]int64{}}
}

func (b *toolBatch) seen(id string) bool {
	_, ok := b.dispatchTS[id]
	return ok
}

// intervalSpan returns the batch's wall clock (max end − min start) and
// whether any two intervals actually overlapped (true parallelism).
func intervalSpan(intervals [][2]int64) (wall int64, overlapped bool) {
	sorted := append([][2]int64(nil), intervals...)
	slices.SortFunc(sorted, func(a, b [2]int64) int {
		switch {
		case a[0] != b[0]:
			return int(a[0] - b[0])
		default:
			return int(a[1] - b[1])
		}
	})
	minStart, maxEnd := sorted[0][0], sorted[0][1]
	for _, iv := range sorted[1:] {
		if iv[0] < maxEnd {
			overlapped = true
		}
		maxEnd = max(maxEnd, iv[1])
	}
	return maxEnd - minStart, overlapped
}

// intervalUnion is the merged length of all intervals, so concurrent tool
// executions count wall-clock once.
func intervalUnion(intervals [][2]int64) int64 {
	sorted := append([][2]int64(nil), intervals...)
	slices.SortFunc(sorted, func(a, b [2]int64) int {
		switch {
		case a[0] != b[0]:
			return int(a[0] - b[0])
		default:
			return int(a[1] - b[1])
		}
	})
	var total int64
	curStart, curEnd := sorted[0][0], sorted[0][1]
	for _, iv := range sorted[1:] {
		if iv[0] > curEnd {
			total += curEnd - curStart
			curStart, curEnd = iv[0], iv[1]
			continue
		}
		curEnd = max(curEnd, iv[1])
	}
	return total + (curEnd - curStart)
}

// durMs renders small durations without the sub-second floor dur applies.
func durMs(ms int64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return dur(ms)
}

func p95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	slices.Sort(sorted)
	index := min((len(sorted)*95+99)/100, len(sorted))
	return sorted[index-1]
}
