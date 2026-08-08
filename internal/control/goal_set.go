package control

// SetGoalDurable updates the Goal only when its sidecar can be replaced
// atomically.
func (c *Controller) SetGoalDurable(goal string) error {
	snapshot := c.goals.capture()
	legacySnapshot, hadLegacySnapshot := c.legacyRestoreSnapshot()
	resolved, setup := c.resolveGoalText(goal, GoalResearchAuto)
	var path string
	var data []byte
	var persist bool
	if setup.blockReason != "" {
		path, data, persist = c.goals.setLegacyArchiveBlocked(resolved, setup.budgetClass, setup.blockReason, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{taskID: setup.legacyTaskID, epoch: c.goals.continuationToken(), explicit: setup.explicit})
	} else {
		path, data, persist = c.goals.set(resolved, setup.budgetClass, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{})
	}
	if persist {
		if err := c.goals.writeStateErr(path, data); err != nil {
			c.goals.restore(snapshot)
			if hadLegacySnapshot {
				legacySnapshot.epoch = c.goals.continuationToken()
				c.replaceLegacyRestore(legacySnapshot)
			} else {
				c.replaceLegacyRestore(legacyGoalRestore{})
			}
			return err
		}
	}
	if setup.notice != "" {
		c.notice(setup.notice)
	}
	if setup.blockReason != "" {
		c.notice("legacy research archive resume failed: " + setup.blockReason)
	}
	return nil
}

func (c *Controller) SetGoalWithResearchMode(goal string, researchMode GoalResearchMode) {
	resolved, setup := c.resolveGoalText(goal, researchMode)
	if setup.notice != "" {
		c.notice(setup.notice)
	}
	var path string
	var data []byte
	var ok bool
	if setup.blockReason != "" {
		path, data, ok = c.goals.setLegacyArchiveBlocked(resolved, setup.budgetClass, setup.blockReason, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{taskID: setup.legacyTaskID, epoch: c.goals.continuationToken(), explicit: setup.explicit})
		c.notice("legacy research archive resume failed: " + setup.blockReason)
	} else {
		path, data, ok = c.goals.set(resolved, setup.budgetClass, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{})
	}
	c.persistGoalState(path, data, ok)
}

// goalSetSetup is the resolved objective and budget class after archive lookup.
type goalSetSetup struct {
	budgetClass  string
	notice       string
	blockReason  string
	legacyTaskID string
	explicit     bool
}

func (c *Controller) resolveGoalText(goal string, researchMode GoalResearchMode) (string, goalSetSetup) {
	setup := goalSetSetup{budgetClass: budgetClassForLegacyMode(goal, researchMode)}
	legacy := c.prepareLegacyResearchTask(goal)
	if !legacy.explicit {
		return goal, setup
	}
	setup.notice, setup.blockReason, setup.legacyTaskID, setup.explicit = legacy.notice, legacy.blockReason, legacy.taskID, legacy.explicit
	if legacy.blockReason != "" {
		return goal, setup
	}
	setup.budgetClass = budgetClassResearch
	return legacy.goal, setup
}
