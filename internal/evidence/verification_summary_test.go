package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVerificationCommandSummaryRecommendationsAreRecognized guards the
// single structured source used to render the summary and exercise the
// classifier. Every concrete example must render and remain accepted.
func TestVerificationCommandSummaryRecommendationsAreRecognized(t *testing.T) {
	summary := VerificationCommandSummary()
	for _, recommendation := range verificationCommandRecommendations() {
		for _, command := range recommendation.examples {
			if !strings.Contains(summary, command) {
				t.Errorf("summary family %q missing concrete example %q: %s", recommendation.label, command, summary)
			}
			if !IsDeliveryVerificationCommand(command) {
				t.Errorf("summary family %q advertises %q, but classifier rejects it", recommendation.label, command)
			}
		}
	}
}

func TestGoBuildAlwaysCountsAsMutation(t *testing.T) {
	// Even ./... can expand to one main package and write a root executable.
	// The static classifier cannot know package expansion or inherited GOFLAGS,
	// so every go build form must fail closed as a mutation.
	for _, command := range []string{
		"go build",
		"go build .",
		"go build ./cmd/reasonix",
		"go build -race ./cmd/reasonix",
		"go build ./...",
		"go build -tags integration ./...",
		"go build -tags=integration ./...",
		"go build -race -trimpath ./...",
		"go build -p 2 -mod=readonly ./...",
		"go build -buildvcs ./...",
		"go build -buildvcs=auto -- ./...",
		"go build -tags ./... ./cmd/reasonix",
		"go build -coverpkg ./... ./cmd/reasonix",
		"go build -pkgdir ./... ./cmd/reasonix",
		"go build -pkgdir=./... ./cmd/reasonix",
		"go build -o reasonix ./...",
		"go build -o=reasonix ./...",
		"go build -mod=mod ./...",
		"go build -n ./...",
		"go build -work ./...",
		"go build -future-flag ./...",
		"go build ./... -o reasonix",
	} {
		if IsDeliveryVerificationCommand(command) {
			t.Errorf("%q must not count as non-mutating verification", command)
		}
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		if !ToolCallMutates("bash", args, false) {
			t.Errorf("%q must be classified as a mutation", command)
		}
	}
	if strings.Contains(VerificationCommandSummary(), "go build") {
		t.Fatal("recovery summary must not recommend go build as a first-line verifier")
	}
}

func TestTSCVerificationRequiresExplicitNoEmit(t *testing.T) {
	for _, command := range []string{
		"tsc --noEmit",
		"tsc --noEmit=true",
		"tsc --project tsconfig.json --noEmit",
	} {
		if !IsDeliveryVerificationCommand(command) {
			t.Errorf("%q should be recognized as an explicit no-emit type check", command)
		}
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		if ToolCallMutates("bash", args, false) {
			t.Errorf("%q should remain a non-mutating verification", command)
		}
	}

	for _, command := range []string{
		"tsc",
		"tsc --outDir dist",
		"tsc --noEmit=false",
		"tsc --noEmit false",
		"tsc --noEmit=true --noEmit=false",
	} {
		if IsDeliveryVerificationCommand(command) {
			t.Errorf("%q may emit compiler output and must not count as verification", command)
		}
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		if !ToolCallMutates("bash", args, false) {
			t.Errorf("%q may emit compiler output and must be classified as a mutation", command)
		}
	}

	if !strings.Contains(VerificationCommandSummary(), "tsc --noEmit") {
		t.Fatal("recovery summary must render the concrete no-emit TypeScript command")
	}
}

// TestVerificationCommandSummaryNamesTheDeadlockTraps ensures the summary
// explicitly warns about the two command shapes that used to send models into
// the readiness deadlock loop: inline interpreters (blocked before execution)
// and read-only inspection commands (executed but never classified as
// verification).
func TestVerificationCommandSummaryNamesTheDeadlockTraps(t *testing.T) {
	s := VerificationCommandSummary()
	for _, want := range []string{
		"python -c",
		"node -e",
		"grep/find/cat/wc",
		"NOT verification",
		"blocked in delivery mode",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary should warn about trap %q, got: %s", want, s)
		}
	}
}

// TestVerificationCommandSummaryAcceptedPipeline ensures the escape hatch
// advertised by the summary (a read-only extraction pipeline ending in a
// recognized verifier) really is accepted by the classifier.
func TestVerificationCommandSummaryAcceptedPipeline(t *testing.T) {
	for _, cmd := range []string{
		"tail -n +1 out.json | node --check -",
		"cat file.js | node --check -",
	} {
		if !IsDeliveryVerificationCommand(cmd) {
			t.Errorf("advertised read-only pipeline %q rejected by classifier", cmd)
		}
	}
}
