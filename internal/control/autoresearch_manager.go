package control

// legacyResearchArchive is a read-only compatibility boundary for Goal
// sidecars and prompts that still reference an old .reasonix/autoresearch
// task. New Goal runs never create, update, list, or expose those archives.

import (
	"log/slog"
	"strings"

	"reasonix/internal/autoresearch"
	"reasonix/internal/evidence"
)

type legacyResearchSetup struct {
	// goal is the original objective recovered from task_spec.json when the
	// user named an explicit archive path. Empty when no archive was referenced.
	goal        string
	taskID      string
	blockReason string
	notice      string
	explicit    bool
}

type legacyResearchArchive struct {
	store *autoresearch.Store
}

// prepare reads an explicitly referenced legacy task. It has no create path
// and never mutates the archive, even when validation fails.
func (m legacyResearchArchive) prepare(goal string) legacyResearchSetup {
	taskID, found, parseErr := autoresearch.ExplicitTaskID(goal)
	if !found {
		return legacyResearchSetup{}
	}
	if parseErr != nil {
		return legacyResearchSetup{explicit: true, blockReason: parseErr.Error()}
	}
	if m.store == nil {
		return legacyResearchSetup{
			explicit:    true,
			taskID:      taskID,
			blockReason: "legacy research archive is unavailable for this workspace",
		}
	}
	task, err := m.store.LoadTask(taskID)
	if err != nil {
		slog.Warn("controller: resume legacy autoresearch task", "err", err)
		return legacyResearchSetup{explicit: true, taskID: taskID, blockReason: err.Error()}
	}
	original := strings.TrimSpace(task.Spec.Goal)
	if original == "" {
		return legacyResearchSetup{
			explicit:    true,
			taskID:      task.ID,
			blockReason: "legacy research archive is missing goal text",
		}
	}
	return legacyResearchSetup{
		goal:     original,
		taskID:   task.ID,
		notice:   "legacy research archive loaded: " + task.ID,
		explicit: true,
	}
}

// loadGoalText returns the original objective stored in a historical archive.
func (m legacyResearchArchive) loadGoalText(taskID string) (string, error) {
	if m.store == nil {
		return "", errLegacyArchiveUnavailable
	}
	task, err := m.store.LoadTask(taskID)
	if err != nil {
		return "", err
	}
	goal := strings.TrimSpace(task.Spec.Goal)
	if goal == "" {
		return "", errLegacyArchiveMissingGoal
	}
	return goal, nil
}

var (
	errLegacyArchiveUnavailable = errString("legacy research archive is unavailable for this workspace")
	errLegacyArchiveMissingGoal = errString("legacy research archive is missing goal text")
)

type errString string

func (e errString) Error() string { return string(e) }

func (c *Controller) prepareLegacyResearchTask(goal string) legacyResearchSetup {
	return c.legacyResearchArchive.prepare(goal)
}

func (c *Controller) restorePendingLegacyGoal(legacy legacyGoalRestore) bool {
	if legacy.taskID == "" {
		goal, epoch, ok := c.goals.legacyArchiveBlockedState()
		if ok {
			setup := c.prepareLegacyResearchTask(goal)
			if setup.explicit && setup.taskID != "" {
				legacy = legacyGoalRestore{taskID: setup.taskID, epoch: epoch, explicit: true}
			}
		}
	}
	if legacy.taskID == "" || (strings.TrimSpace(c.goals.goalText()) != "" && !legacy.explicit) {
		c.replaceLegacyRestore(legacyGoalRestore{})
		return false
	}
	c.replaceLegacyRestore(legacy)
	restoreTodos := c.goalTodos()
	if len(legacy.todos) > 0 {
		restoreTodos = append([]evidence.TodoItem(nil), legacy.todos...)
		if c.executor != nil {
			c.executor.ReplaceTodoState(restoreTodos)
		}
	}
	goal, err := c.legacyResearchArchive.loadGoalText(legacy.taskID)
	if err != nil {
		if epoch, ok := c.goals.blockLegacyRestore(legacy.epoch, err.Error()); ok {
			c.advanceLegacyRestoreEpoch(legacy.taskID, legacy.epoch, epoch)
			c.notice("legacy research archive resume failed: " + err.Error())
		}
		return true
	}
	if legacy.explicit {
		if epoch, ok := c.goals.resumeLegacyArchive(legacy.epoch, goal); ok {
			c.persistGoalStateAtEpoch(epoch, restoreTodos)
			c.clearLegacyRestore(legacy.taskID, legacy.epoch)
		}
		return true
	}
	if strings.TrimSpace(c.goals.goalText()) == "" {
		if epoch, ok := c.goals.fillGoalTextIfEmpty(legacy.epoch, goal); ok {
			c.persistGoalStateAtEpoch(epoch, restoreTodos)
			c.clearLegacyRestore(legacy.taskID, legacy.epoch)
		}
	}
	return true
}

func (c *Controller) retryBlockedLegacyGoal() (handled, resumed bool) {
	legacy, ok := c.legacyRestoreSnapshot()
	if !ok {
		return false, false
	}
	goal, ok := c.goals.legacyArchiveRetryToken(legacy.epoch)
	if !ok {
		c.clearLegacyRestore(legacy.taskID, legacy.epoch)
		return false, false
	}
	taskID, epoch := legacy.taskID, legacy.epoch
	setup := c.prepareLegacyResearchTask(goal)
	resolvedGoal := setup.goal
	if !setup.explicit {
		var err error
		resolvedGoal, err = c.legacyResearchArchive.loadGoalText(taskID)
		if err != nil {
			setup.blockReason = err.Error()
		} else if strings.TrimSpace(goal) != "" {
			resolvedGoal = goal
		}
	}
	if setup.blockReason != "" || strings.TrimSpace(resolvedGoal) == "" {
		reason := setup.blockReason
		if reason == "" {
			reason = "legacy research archive could not be recovered"
		}
		c.notice("legacy research archive resume failed: " + reason)
		return true, false
	}
	todos := c.goalTodos()
	resumedEpoch, applied := c.goals.resumeLegacyArchive(epoch, resolvedGoal)
	if !applied {
		return true, false
	}
	c.persistGoalStateAtEpoch(resumedEpoch, todos)
	c.clearLegacyRestore(taskID, epoch)
	c.notice(setup.notice)
	if c.executor != nil {
		c.executor.RestoreDeliveryCheckpoint(c.goals.deliveryState())
	}
	return true, true
}
