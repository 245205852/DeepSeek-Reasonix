package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVerificationCommandSummaryRecommendationsAreRecognized guards the
// single structured source used to render the summary and exercise the
// classifier. Every advertised family must render and every example attached
// to that family must remain accepted.
func TestVerificationCommandSummaryRecommendationsAreRecognized(t *testing.T) {
	summary := VerificationCommandSummary()
	for _, recommendation := range verificationCommandRecommendations() {
		if !strings.Contains(summary, recommendation.label) {
			t.Errorf("summary missing recommended family %q: %s", recommendation.label, summary)
		}
		for _, command := range recommendation.examples {
			if !IsDeliveryVerificationCommand(command) {
				t.Errorf("summary family %q advertises %q, but classifier rejects it", recommendation.label, command)
			}
		}
	}
}

func TestGoBuildVerificationRequiresSafeAllPackageOperands(t *testing.T) {
	for _, command := range []string{
		"go build ./...",
		"go build -tags integration ./...",
		"go build -tags=integration ./...",
		"go build -race -trimpath ./...",
		"go build -p 2 -mod=readonly ./...",
		"go build -buildvcs ./...",
		"go build -buildvcs=auto -- ./...",
	} {
		if !IsDeliveryVerificationCommand(command) {
			t.Errorf("%q should remain recognized all-package verification", command)
		}
	}
	if ToolCallMutates("bash", json.RawMessage(`{"command":"go build ./..."}`), false) {
		t.Fatal("go build ./... should remain non-mutating all-package verification")
	}
	for _, command := range []string{
		"go build",
		"go build .",
		"go build ./cmd/reasonix",
		"go build -race ./cmd/reasonix",
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
