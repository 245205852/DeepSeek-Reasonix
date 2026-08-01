package evidence

import (
	"strings"
	"testing"
)

// TestVerificationCommandSummaryConsistentWithClassifier guards against drift
// between the model-readable summary embedded in final-readiness messages and
// the classifier that actually decides whether a command counts as
// verification. Every command family named in the summary must be accepted by
// IsDeliveryVerificationCommand; if the classifier grows or shrinks, this test
// forces the summary to be updated in the same change.
func TestVerificationCommandSummaryConsistentWithClassifier(t *testing.T) {
	accepted := []string{
		"go test ./...",
		"go vet ./...",
		"go build ./cmd/reasonix",
		"git diff --check",
		"pytest tests/",
		"gotestsum",
		"staticcheck ./...",
		"golangci-lint run",
		"tsc --noEmit",
		"mypy src/",
		"npm test",
		"pnpm check",
		"yarn lint",
		"bun test",
		"npm run typecheck",
		"cargo test",
		"cargo clippy",
		"npx vitest run",
		"npx jest --runInBand",
		"node --check index.js",
		"node --test",
		"make test",
		"just verify",
		"python -m pytest",
		"python -m unittest",
		"python -m compileall .",
		"dotnet test",
		"mvn test",
		"gradle check",
	}
	for _, cmd := range accepted {
		if !IsDeliveryVerificationCommand(cmd) {
			t.Errorf("summary claims %q is recognized verification, but classifier rejects it", cmd)
		}
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
