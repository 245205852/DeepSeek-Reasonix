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
// nice-to-have that must not block completion.
type Requirement struct {
	ID       string
	Text     string
	Required bool
	Status   Status
	Evidence []EvidenceRef
}

// Check is an expected verification; an empty Command accepts any
// verification-classified command.
type Check struct {
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
		if !checkMatches(c.Checks[i].Command, r) {
			continue
		}
		c.Checks[i].Evidence = append(c.Checks[i].Evidence, ref)
		if r.Success {
			c.Checks[i].Status = Satisfied
		} else if c.Checks[i].Status != Satisfied {
			c.Checks[i].Status = Failed
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

func checkMatches(want string, r evidence.Receipt) bool {
	if r.Command == "" {
		return false
	}
	if want == "" {
		return evidence.IsDeliveryVerificationCommand(r.Command)
	}
	return evidence.CommandMatches(want, r.Command)
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
