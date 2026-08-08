package control

import "reasonix/internal/evidence"

// goalMachineSnapshot is an in-memory rollback point for durable Goal updates.
// Persistence paths and mutexes are deliberately excluded.
type goalMachineSnapshot struct {
	goal                   string
	status                 string
	scopeID                string
	deliveryCheckpoint     evidence.DeliveryCheckpoint
	block                  string
	strict                 bool
	budgetClass            string
	turnsUsed              int
	turnsLimit             int
	tokensUsed             int
	tokensLimit            int
	noProgressTurns        int
	noProgressLimit        int
	lastContinuationReason string
	lastEvaluatorReason    string
	stopCause              string
	budgetExtensions       int
}

func (g *goalMachine) capture() goalMachineSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return goalMachineSnapshot{
		goal: g.goal, status: g.status,
		scopeID: g.scopeID, deliveryCheckpoint: g.deliveryCheckpoint,
		block: g.block, strict: g.strict,
		budgetClass: g.budgetClass, turnsUsed: g.turnsUsed,
		turnsLimit: g.turnsLimit, tokensUsed: g.tokensUsed,
		tokensLimit: g.tokensLimit, noProgressTurns: g.noProgressTurns,
		noProgressLimit:        g.noProgressLimit,
		lastContinuationReason: g.lastContinuationReason,
		lastEvaluatorReason:    g.lastEvaluatorReason,
		stopCause:              g.stopCause, budgetExtensions: g.budgetExtensions,
	}
}

func (g *goalMachine) restore(snapshot goalMachineSnapshot) {
	g.mu.Lock()
	g.goal, g.status = snapshot.goal, snapshot.status
	g.scopeID = snapshot.scopeID
	g.deliveryCheckpoint, g.block = snapshot.deliveryCheckpoint, snapshot.block
	g.strict = snapshot.strict
	g.budgetClass = snapshot.budgetClass
	g.turnsUsed, g.turnsLimit = snapshot.turnsUsed, snapshot.turnsLimit
	g.tokensUsed, g.tokensLimit = snapshot.tokensUsed, snapshot.tokensLimit
	g.noProgressTurns, g.noProgressLimit = snapshot.noProgressTurns, snapshot.noProgressLimit
	g.lastContinuationReason = snapshot.lastContinuationReason
	g.lastEvaluatorReason = snapshot.lastEvaluatorReason
	g.stopCause = snapshot.stopCause
	g.budgetExtensions = snapshot.budgetExtensions
	g.continuationEpoch++
	g.mu.Unlock()
}
