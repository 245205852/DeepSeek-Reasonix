package cli

import (
	"regexp"
	"strings"
	"testing"

	"reasonix/internal/event"
)

var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func cardText(t *testing.T, r *event.CompletionReceipt) string {
	t.Helper()
	return ansiSequence.ReplaceAllString(strings.Join(renderReceiptCard(r, 100), "\n"), "")
}

// The user just watched the tools run; repeating the work back is noise. What
// the transcript cannot carry is the absence, so the clean case gets one line.
func TestCleanReceiptIsOneQuietLine(t *testing.T) {
	got := cardText(t, &event.CompletionReceipt{
		Verdict:       "done",
		Changes:       []event.ReceiptChange{{Path: "calc.py", Reviewed: true}},
		Verifications: []event.ReceiptVerification{{Command: "go test ./...", Passed: true}},
	})
	if lines := strings.Count(got, "\n"); lines != 0 {
		t.Fatalf("clean receipt used %d lines:\n%s", lines+1, got)
	}
	if !strings.Contains(got, "1 changed") || !strings.Contains(got, "go test ./...") {
		t.Fatalf("the clean line must name its evidence, got %q", got)
	}
}

func TestReceiptSpendsItsLinesOnWhatIsMissing(t *testing.T) {
	got := cardText(t, &event.CompletionReceipt{
		Verdict: "partial",
		Changes: []event.ReceiptChange{{Path: "calc.py"}},
		Gaps: []event.ReceiptGap{
			{Kind: "unreviewed_change", Detail: "calc.py"},
			{Kind: "stale_verification", Detail: "go test ./..."},
		},
	})
	for _, want := range []string{"calc.py", "go test ./..."} {
		if !strings.Contains(got, want) {
			t.Fatalf("card missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("want a header and two gap lines:\n%s", got)
	}
}

// Dropping one silently is the failure this card exists to prevent.
func TestUnknownGapKindIsStillShown(t *testing.T) {
	got := cardText(t, &event.CompletionReceipt{
		Verdict: "partial",
		Gaps:    []event.ReceiptGap{{Kind: "kind_from_a_newer_kernel", Detail: "x.go"}},
	})
	if !strings.Contains(got, "kind_from_a_newer_kernel") || !strings.Contains(got, "x.go") {
		t.Fatalf("an unknown kind must still reach the user:\n%s", got)
	}
}

func TestLongGapListIsBoundedAndSaysSo(t *testing.T) {
	var gaps []event.ReceiptGap
	for i := range 9 {
		gaps = append(gaps, event.ReceiptGap{Kind: "unreviewed_change", Detail: string(rune('a'+i)) + ".go"})
	}
	got := cardText(t, &event.CompletionReceipt{Verdict: "partial", Gaps: gaps})
	if strings.Count(got, ".go") != maxReceiptGapLines {
		t.Fatalf("want exactly %d gap lines:\n%s", maxReceiptGapLines, got)
	}
	if !strings.Contains(got, "4") {
		t.Fatalf("the dropped count must be stated, not hidden:\n%s", got)
	}
}

func TestDeclaredRisksSurvive(t *testing.T) {
	got := cardText(t, &event.CompletionReceipt{
		Verdict: "partial",
		Gaps:    []event.ReceiptGap{{Kind: "declared_unverified", Detail: "desktop UI"}},
		Risks:   []string{"the migration is one-way"},
	})
	if !strings.Contains(got, "the migration is one-way") {
		t.Fatalf("a declared risk must reach the user:\n%s", got)
	}
}

func TestNoCardWithoutAReceiptOrAVerdict(t *testing.T) {
	if got := renderReceiptCard(nil, 80); got != nil {
		t.Fatalf("nil receipt rendered %v", got)
	}
	if got := renderReceiptCard(&event.CompletionReceipt{Verdict: "incomplete"}, 80); got != nil {
		t.Fatalf("an incomplete turn with no gaps has nothing to say, got %v", got)
	}
}
