package main

import (
	"strings"
	"testing"
)

func TestMarkDominatedFindsTheAlarm(t *testing.T) {
	points := []paretoPoint{
		{label: "fast-accurate", acc: 90, ttcsMs: 30_000, solved: 9, ran: 10},
		{label: "reasonix", acc: 70, ttcsMs: 100_000, solved: 7, ran: 10},
		{label: "slow-accurate", acc: 95, ttcsMs: 150_000, solved: 19, ran: 20},
	}
	markDominated(points)
	if points[1].dominatedBy != "fast-accurate" {
		t.Errorf("reasonix dominatedBy = %q, want fast-accurate (more accurate and faster)", points[1].dominatedBy)
	}
	if points[0].dominatedBy != "" || points[2].dominatedBy != "" {
		t.Errorf("frontier points must not be dominated: %q / %q", points[0].dominatedBy, points[2].dominatedBy)
	}
}

func TestMarkDominatedIgnoresUnsolvedArms(t *testing.T) {
	points := []paretoPoint{
		{label: "a", acc: 50, ttcsMs: 60_000, solved: 1, ran: 2},
		{label: "none", acc: 0, ttcsMs: 0, solved: 0, ran: 2},
	}
	markDominated(points)
	if points[0].dominatedBy != "" {
		t.Errorf("a zero-solve arm must not dominate (ttcs 0 is absence, not speed): %q", points[0].dominatedBy)
	}
}

func TestParetoSectionRendersChartAndVerdicts(t *testing.T) {
	got := paretoSection([]paretoPoint{
		{label: "a", acc: 90, ttcsMs: 30_000, solved: 9, ran: 10},
		{label: "b", acc: 70, ttcsMs: 100_000, solved: 7, ran: 10},
		{label: "none", acc: 0, ttcsMs: 0, solved: 0, ran: 2},
	})
	for _, want := range []string{
		"### Pareto: accuracy vs TTCS",
		"Accuracy",
		"→ TTCS",
		"A=a", "✗",
		"✅ `a` is on the Pareto frontier",
		"⚠️ `b` is **dominated** by `a`",
		"`none`: no solves",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pareto section missing %q:\n%s", want, got)
		}
	}
	if paretoSection([]paretoPoint{{label: "solo", acc: 50, ttcsMs: 1000, solved: 1, ran: 2}}) != "" {
		t.Fatal("a single arm has no comparison to draw")
	}
}
