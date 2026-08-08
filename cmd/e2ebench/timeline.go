package main

import (
	"fmt"
	"strings"
)

const timelineWidth = 48

// renderTimelines draws each checkpointed run's lifecycle as the argument-
// ending picture: start → first useful mutation → CORRECT → final, with the
// before/after-correct tallies underneath.
func renderTimelines(results []result) string {
	var b strings.Builder
	for _, r := range results {
		if len(r.Checkpoints) == 0 || r.WallMs == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("### Timelines\n\n")
		}
		fmt.Fprintf(&b, "`%s`\n\n```\n%s```\n\n", r.ID, taskTimeline(r))
	}
	return b.String()
}

func taskTimeline(r result) string {
	pos := func(ms int64) int {
		p := int(ms * timelineWidth / r.WallMs)
		return min(max(p, 0), timelineWidth)
	}
	markers := []struct {
		at    int
		label string
	}{{0, "start"}}
	if r.FirstUsefulMs > 0 {
		markers = append(markers, struct {
			at    int
			label string
		}{pos(r.FirstUsefulMs), "useful mutation " + dur(r.FirstUsefulMs)})
	}
	if r.FirstCorrectMs > 0 {
		markers = append(markers, struct {
			at    int
			label string
		}{pos(r.FirstCorrectMs), "CORRECT " + dur(r.FirstCorrectMs)})
	}
	markers = append(markers, struct {
		at    int
		label string
	}{timelineWidth, "final " + dur(r.WallMs)})

	bar := []rune(strings.Repeat("─", timelineWidth+1))
	for _, m := range markers {
		bar[m.at] = '│'
	}
	out := string(bar) + "\n"
	for _, m := range markers {
		out += strings.Repeat(" ", m.at) + "^ " + m.label + "\n"
	}
	if r.Passed && r.PostSolveWasteMs > 0 {
		out += fmt.Sprintf("post-solve waste %s (%s of wall)\n", dur(r.PostSolveWasteMs), pct(int(r.PostSolveWasteMs), int(r.WallMs)))
	}
	if r.FirstCorrectMs > 0 {
		out += fmt.Sprintf("rounds %d→%d · calls %d→%d · after correct: verifications %d · reviews %d · mutations %d\n",
			r.RoundsBeforeCorrect, r.RoundsAfterCorrect,
			r.CallsBeforeCorrect, r.CallsAfterCorrect,
			r.VerifyAfterCorrect, r.ReviewsAfterCorrect, r.MutationsAfterCorrect)
	}
	return out
}
