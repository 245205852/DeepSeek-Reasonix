package evidence

import (
	"path"
	"sort"
	"strings"

	"reasonix/internal/shellsafe"
)

// OutcomeSample decomposes one tool round's receipts by outcome: information
// gathered (Exploration), verification attempts run, and verification-command
// state transitions (Objective fail→pass, Regression pass→fail). Counts are
// unit-weighted; policy weighting is an offline concern.
type OutcomeSample struct {
	Round        int
	Exploration  int
	Verification int
	Objective    int
	Regression   int
	Churn        int
	// LegacyGain is the live novelty scorer's verdict on the same receipts, so
	// offline analysis can compare the two policies without replaying.
	LegacyGain int
	// Discriminating counts observations able to falsify the working
	// hypothesis: verification commands, or commands exercising a mutated
	// file — deliberately broader than delivery verification (repro scripts).
	Discriminating int
	// DebtAge counts consecutive rounds carrying an unverified mutation with
	// no discriminating observation; 0 while no verification debt is open.
	DebtAge int
	// BlindMutations counts mutations since the last discriminating
	// observation — the EBM policy's trigger input.
	BlindMutations int
	// EBMEligible/EBMFired mark the Evidence-Before-More-Mutation trigger
	// holding and its nudge firing; the agent stamps both so every arm —
	// baseline included — carries the eligibility shadow.
	EBMEligible bool
	EBMFired    bool
}

// OutcomeTracker is the shadow counterpart of ProgressTracker: same per-round
// receipts, scored by outcome instead of novelty. It never influences guard
// behavior — samples exist only for trajectory recording and offline analysis.
type OutcomeTracker struct {
	legacy       *ProgressTracker
	round        int
	readPaths    map[string]bool
	commands     map[string]bool
	failures     map[string]bool
	actions      map[string]bool
	verifySeen   map[string]bool
	verifyPass   map[string]bool
	mutatedBases map[string]bool
	debt         bool
	debtAge      int
	blind        int
}

// ForkSeed exports the debt-relevant state a counterfactual fork must carry so
// post-fork discriminating detection stays continuous with the original run.
func (t *OutcomeTracker) ForkSeed() (mutatedBases []string, debtAge, blindMutations int) {
	for base := range t.mutatedBases {
		mutatedBases = append(mutatedBases, base)
	}
	sort.Strings(mutatedBases)
	return mutatedBases, t.debtAge, t.blind
}

// RestoreOutcomeTracker rebuilds a tracker from a fork seed. Novelty maps
// start empty — post-fork exploration novelty is intentionally relative to the
// fork point, while debt state continues from the original trajectory.
func RestoreOutcomeTracker(mutatedBases []string, debtAge, blindMutations int) *OutcomeTracker {
	t := NewOutcomeTracker()
	for _, base := range mutatedBases {
		t.mutatedBases[base] = true
	}
	t.debtAge = debtAge
	t.blind = blindMutations
	t.debt = debtAge > 0 || blindMutations > 0
	return t
}

func NewOutcomeTracker() *OutcomeTracker {
	return &OutcomeTracker{
		legacy:       NewProgressTracker(),
		readPaths:    map[string]bool{},
		commands:     map[string]bool{},
		failures:     map[string]bool{},
		actions:      map[string]bool{},
		verifySeen:   map[string]bool{},
		verifyPass:   map[string]bool{},
		mutatedBases: map[string]bool{},
	}
}

// ScoreRound folds one round's receipts into the tracker and returns the
// round's outcome decomposition.
func (t *OutcomeTracker) ScoreRound(receipts []Receipt) OutcomeSample {
	if t == nil {
		return OutcomeSample{}
	}
	t.round++
	s := OutcomeSample{Round: t.round}
	for _, r := range receipts {
		t.scoreReceipt(r, &s)
	}
	s.LegacyGain = t.legacy.ScoreRound(receipts)
	// Verification debt: a discriminating observation settles it; otherwise a
	// mutation opens it and every silent round ages it, mutation round included.
	if s.Discriminating > 0 {
		t.debt, t.debtAge, t.blind = false, 0, 0
	} else {
		if s.Churn > 0 {
			t.debt = true
			t.blind += s.Churn
		}
		if t.debt {
			t.debtAge++
		}
	}
	s.DebtAge = t.debtAge
	s.BlindMutations = t.blind
	return s
}

// noteMutatedPaths remembers mutated file basenames so a later command that
// mentions one (running a repro script, a targeted test file) reads as a
// discriminating observation even when it is not delivery verification.
func (t *OutcomeTracker) noteMutatedPaths(paths []string) {
	for _, p := range paths {
		if base := path.Base(strings.ReplaceAll(p, "\\", "/")); len(base) >= 3 {
			t.mutatedBases[base] = true
		}
	}
}

func (t *OutcomeTracker) commandExercisesMutation(command string) bool {
	// Inspecting a mutated file (cat/grep/head) cannot falsify anything; only
	// a command that can execute it discriminates.
	if _, _, readOnly := shellsafe.CommandIsReadOnly(command); readOnly {
		return false
	}
	for base := range t.mutatedBases {
		if strings.Contains(command, base) {
			return true
		}
	}
	return false
}

func (t *OutcomeTracker) scoreReceipt(r Receipt, s *OutcomeSample) {
	if command := strings.TrimSpace(r.Command); command != "" {
		t.scoreCommand(command, r, s)
		return
	}
	switch {
	case r.Success && (r.Mutation || r.Write):
		// A mutation is a state transition, not proof of progress: it counts
		// as churn until a verification transition vouches for it.
		s.Churn++
		t.noteMutatedPaths(r.Paths)
	case r.Success && (r.ToolName == "task" || r.ToolName == "parallel_tasks" || r.ToolName == "fleet"):
		// A delegation return is new information at best — never objective
		// progress on its own.
		s.Exploration++
	case r.Success && (r.StepProof || r.TodoStep != nil || len(r.Todos) > 0):
		// Bookkeeping moves no outcome dimension.
	case r.Success && r.Read && r.OutputBytes > 0 && len(r.Paths) > 0:
		for _, path := range r.Paths {
			if path == "" || t.readPaths[path] {
				continue
			}
			t.readPaths[path] = true
			s.Exploration++
		}
	case r.Success:
		sig := r.ToolName + "\x00" + string(r.Args)
		if !t.actions[sig] {
			t.actions[sig] = true
			s.Exploration++
		}
	}
}

func (t *OutcomeTracker) scoreCommand(command string, r Receipt, s *OutcomeSample) {
	if r.Success && (r.Mutation || r.Write) {
		s.Churn++
		t.noteMutatedPaths(r.Paths)
	}
	verify := IsDeliveryVerificationCommand(command)
	if verify || t.commandExercisesMutation(command) {
		s.Discriminating++
	}
	if verify {
		s.Verification++
		seen, wasPass := t.verifySeen[command], t.verifyPass[command]
		t.verifySeen[command] = true
		t.verifyPass[command] = r.Success
		if seen && r.Success && !wasPass {
			s.Objective++
		}
		if seen && !r.Success && wasPass {
			s.Regression++
		}
	}
	if r.Success {
		if !verify && !t.commands[command] {
			s.Exploration++
		}
		t.commands[command] = true
		return
	}
	if !t.failures[command] {
		t.failures[command] = true
		s.Exploration++
	}
}
