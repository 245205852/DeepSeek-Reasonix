package control

import (
	"context"

	"reasonix/internal/tool"
)

// bindTurnScope binds the Goal usage recorder whose span stays active until the
// FSM commits. Goal and ordinary chat install no host-owned round ceiling;
// explicit max_steps remains owned by the caller that configured the Agent.
func (c *Controller) bindTurnScope(ctx context.Context, continuation *goalContinuationSnapshot) context.Context {
	goalScopeID, goalScoped := c.goals.goalScopeIDForTurn(continuation)
	if !goalScoped {
		return ctx
	}
	recorder := c.goals.newTurnRecorder(goalScopeID, c.goals.continuationToken())
	c.goalUsageTee.setActiveRecorder(recorder)
	return tool.WithGoalTurnRecorder(ctx, recorder)
}
