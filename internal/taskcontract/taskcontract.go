// Package taskcontract is the convergence point for the host's task state:
// one Contract assembled purely from signals the runtime already produced —
// the taskintent classification, planner-gate features, plan acceptance
// criteria, and the evidence ledger's receipts. Building or updating a
// contract never makes a model call; every termination arbiter reads the
// same record instead of keeping its own.
package taskcontract

import (
	"fmt"

	"reasonix/internal/evidence"
	"reasonix/internal/taskintent"
)

// Risk is the highest risk any upstream signal assigned to the task.
type Risk uint8

const (
	RiskLow Risk = iota
	RiskMedium
	RiskHigh
)

// Status is a requirement's or check's lifecycle.
type Status uint8

const (
	Pending Status = iota
	Satisfied
	Failed
)

// EvidenceKind classifies what a receipt proved.
type EvidenceKind uint8

const (
	EvidenceRead EvidenceKind = iota
	EvidenceMutation
	EvidenceVerification
	EvidenceReview
)

// EvidenceRef points at one ledger receipt without copying its content.
type EvidenceRef struct {
	Kind          EvidenceKind
	MutationEpoch uint64 // ledger sequence at attach time
	Source        string // tool name
	Success       bool
}

// Requirement is one acceptance criterion; Required=false records a
// nice-to-have that must not block completion. Auto requirements are
// satisfied by the first successful receipt of AutoKind — the opt-in for
// tasks whose ask IS the evidence (fix a typo: the mutation proves it).
type Requirement struct {
	ID       string
	Kind     string // optional taxonomy, e.g. "behavior", "regression"
	Text     string
	Required bool
	Status   Status
	Evidence []EvidenceRef
	Auto     bool
	AutoKind EvidenceKind
}

// CheckKind selects what proves a check: a verification command, or any
// successful workspace mutation (scoped to Scope.Paths when set).
type CheckKind uint8

const (
	CheckCommand CheckKind = iota
	CheckMutation
)

// Check is an expected proof; for CheckCommand an empty Command accepts any
// verification-classified command.
type Check struct {
	Kind     CheckKind
	Command  string
	Status   Status
	Evidence []EvidenceRef
}

// Scope is where the task is expected to act, from prompt-shape signals.
type Scope struct {
	Anchored     bool // names concrete files or targets
	MultiFile    bool
	CrossSurface bool
	Paths        []string
}

// Signals is plain data extracted by callers from machinery this package
// must not import (planner-gate features, delivery risk); merging them here
// keeps the contract the single record without inverting layering.
type Signals struct {
	HighRisk     bool
	MediumRisk   bool
	Anchored     bool
	MultiFile    bool
	CrossSurface bool
	Paths        []string
}

// Contract is the unified task record.
type Contract struct {
	Intent       taskintent.Intent
	Risk         Risk
	Scope        Scope
	Requirements []Requirement
	Checks       []Check

	epoch uint64
}

// New classifies input with the existing taskintent heuristics and returns
// an otherwise empty contract; no model call is made.
func New(input string) *Contract {
	return &Contract{Intent: taskintent.Classify(input)}
}

// Atomic is the zero-overhead contract for a simple ask: the ask itself is
// the one requirement (auto-satisfied by mutation evidence) and the one
// check is "the workspace changed". No planner, no evaluator, no extra
// round — the host synthesizes it and the ledger completes it.
func Atomic(input string) *Contract {
	c := New(input)
	c.Requirements = append(c.Requirements, Requirement{
		ID: "r1", Text: input, Required: true, Auto: true, AutoKind: EvidenceMutation,
	})
	c.Checks = append(c.Checks, Check{Kind: CheckMutation})
	return c
}

// PlanFacts is a completed full plan's contract-relevant output, extracted
// by the coordinator (plain data; this package never sees the plan text).
type PlanFacts struct {
	AcceptanceCriteria []string
	Regressions        []string // must-keep-passing criteria
	Verifications      []string // command-level checks; "" entries mean any
	Risky              bool
	Touchpoints        []string
}

// FromPlan builds the contract straight from a plan the planner already
// produced, so the executor consumes the plan's own acceptance criteria
// instead of re-deriving a parallel set: Planner → Contract → Executor.
func FromPlan(input string, facts PlanFacts) *Contract {
	c := New(input)
	for i, text := range facts.AcceptanceCriteria {
		c.Requirements = append(c.Requirements, Requirement{
			ID: fmt.Sprintf("r%d", i+1), Kind: "behavior", Text: text, Required: true,
		})
	}
	for i, text := range facts.Regressions {
		c.Requirements = append(c.Requirements, Requirement{
			ID: fmt.Sprintf("g%d", i+1), Kind: "regression", Text: text, Required: true,
		})
	}
	for _, command := range facts.Verifications {
		c.AddCheck(command)
	}
	c.MergeSignals(Signals{
		MediumRisk: facts.Risky,
		Anchored:   len(facts.Touchpoints) > 0,
		MultiFile:  len(facts.Touchpoints) > 1,
		Paths:      facts.Touchpoints,
	})
	return c
}

// ExecutionView renders the contract as the executor's todo list — a view,
// not a parallel task description: requirements become steps, checks become
// verify steps, and satisfied entries arrive already completed.
func (c *Contract) ExecutionView() []evidence.TodoItem {
	var todos []evidence.TodoItem
	status := func(s Status) string {
		if s == Satisfied {
			return "completed"
		}
		return "pending"
	}
	for _, req := range c.Requirements {
		if !req.Required {
			continue
		}
		todos = append(todos, evidence.TodoItem{Content: req.Text, Status: status(req.Status)})
	}
	for _, check := range c.Checks {
		label := check.Command
		if label == "" {
			if check.Kind == CheckMutation {
				label = "apply the change"
			} else {
				label = "run verification"
			}
		} else {
			label = "verify: " + label
		}
		todos = append(todos, evidence.TodoItem{Content: label, Status: status(check.Status)})
	}
	return todos
}

// Trivial reports whether the contract is simple enough to route
// executor-only and skip every arbiter beyond the ledger itself.
func (c *Contract) Trivial() bool {
	return c.Risk == RiskLow &&
		!c.Scope.MultiFile && !c.Scope.CrossSurface &&
		len(c.Requirements) <= 1 && len(c.Checks) <= 1
}

// MergeSignals folds prompt-shape and risk signals in; risk only ratchets up.
func (c *Contract) MergeSignals(s Signals) {
	if s.HighRisk {
		c.Risk = RiskHigh
	} else if s.MediumRisk && c.Risk < RiskMedium {
		c.Risk = RiskMedium
	}
	c.Scope.Anchored = c.Scope.Anchored || s.Anchored
	c.Scope.MultiFile = c.Scope.MultiFile || s.MultiFile
	c.Scope.CrossSurface = c.Scope.CrossSurface || s.CrossSurface
	c.Scope.Paths = appendNew(c.Scope.Paths, s.Paths)
}

// AddRequirement records one acceptance criterion (e.g. a plan's acceptance
// criteria or a goal spec requirement). Duplicate IDs update the text.
func (c *Contract) AddRequirement(id, text string, required bool) {
	for i := range c.Requirements {
		if c.Requirements[i].ID == id {
			c.Requirements[i].Text = text
			c.Requirements[i].Required = required
			return
		}
	}
	c.Requirements = append(c.Requirements, Requirement{ID: id, Text: text, Required: required})
}

// AddCheck records an expected verification command ("" = any verification).
func (c *Contract) AddCheck(command string) {
	for _, check := range c.Checks {
		if check.Command == command {
			return
		}
	}
	c.Checks = append(c.Checks, Check{Command: command})
}

// Observe folds one ledger receipt into the contract: it advances the
// mutation epoch, and satisfies any check the receipt's command proves.
// Requirement satisfaction stays an explicit caller judgment (Resolve).
func (c *Contract) Observe(r evidence.Receipt) {
	c.epoch++
	ref := refFor(c.epoch, r)
	for i := range c.Checks {
		if !c.checkMatches(c.Checks[i], r, ref) {
			continue
		}
		c.Checks[i].Evidence = append(c.Checks[i].Evidence, ref)
		if r.Success {
			c.Checks[i].Status = Satisfied
		} else if c.Checks[i].Status != Satisfied {
			c.Checks[i].Status = Failed
		}
	}
	for i := range c.Requirements {
		req := &c.Requirements[i]
		if req.Auto && req.Status != Satisfied && r.Success && ref.Kind == req.AutoKind {
			req.Status = Satisfied
			req.Evidence = append(req.Evidence, ref)
		}
	}
}

// Resolve sets a requirement's status with the evidence that justified it.
func (c *Contract) Resolve(id string, status Status, refs ...EvidenceRef) bool {
	for i := range c.Requirements {
		if c.Requirements[i].ID == id {
			c.Requirements[i].Status = status
			c.Requirements[i].Evidence = append(c.Requirements[i].Evidence, refs...)
			return true
		}
	}
	return false
}

// Epoch is the count of receipts observed so far.
func (c *Contract) Epoch() uint64 { return c.epoch }

// Complete reports whether every required requirement and every check is
// satisfied — the one answer the termination arbiters share.
func (c *Contract) Complete() bool {
	for _, req := range c.Requirements {
		if req.Required && req.Status != Satisfied {
			return false
		}
	}
	for _, check := range c.Checks {
		if check.Status != Satisfied {
			return false
		}
	}
	return true
}

// Outstanding lists what still blocks completion, requirements first.
func (c *Contract) Outstanding() []string {
	var out []string
	for _, req := range c.Requirements {
		if req.Required && req.Status != Satisfied {
			out = append(out, fmt.Sprintf("requirement %s: %s", req.ID, req.Text))
		}
	}
	for _, check := range c.Checks {
		if check.Status != Satisfied {
			label := check.Command
			if label == "" {
				label = "any verification"
			}
			out = append(out, "check: "+label)
		}
	}
	return out
}

func refFor(epoch uint64, r evidence.Receipt) EvidenceRef {
	kind := EvidenceRead
	switch {
	case r.ToolName == "review_report":
		kind = EvidenceReview
	case r.Command != "" && evidence.IsDeliveryVerificationCommand(r.Command):
		kind = EvidenceVerification
	case r.Mutation || r.Write:
		kind = EvidenceMutation
	}
	return EvidenceRef{Kind: kind, MutationEpoch: epoch, Source: r.ToolName, Success: r.Success}
}

func (c *Contract) checkMatches(check Check, r evidence.Receipt, ref EvidenceRef) bool {
	if check.Kind == CheckMutation {
		if ref.Kind != EvidenceMutation {
			return false
		}
		if len(c.Scope.Paths) == 0 {
			return true
		}
		return pathsIntersect(c.Scope.Paths, r.Paths)
	}
	if r.Command == "" {
		return false
	}
	if check.Command == "" {
		return evidence.IsDeliveryVerificationCommand(r.Command)
	}
	return evidence.CommandMatches(check.Command, r.Command)
}

func pathsIntersect(scope, got []string) bool {
	for _, s := range scope {
		for _, g := range got {
			if s == g {
				return true
			}
		}
	}
	return false
}

func appendNew(dst, add []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, p := range dst {
		seen[p] = true
	}
	for _, p := range add {
		if !seen[p] {
			seen[p] = true
			dst = append(dst, p)
		}
	}
	return dst
}
