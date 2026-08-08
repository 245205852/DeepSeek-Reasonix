package control

// legacyResearchArchive is a read-only compatibility boundary for Goal
// sidecars and prompts that still reference an old .reasonix/autoresearch
// task. New Goal runs never create, update, list, or expose those archives.

import (
	"log/slog"
	"strings"

	"reasonix/internal/autoresearch"
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
	if m.store == nil {
		if _, ok := autoresearch.ExplicitTaskID(goal); ok {
			return legacyResearchSetup{
				explicit:    true,
				blockReason: "legacy research archive is unavailable for this workspace",
			}
		}
		return legacyResearchSetup{}
	}
	task, ok, err := m.store.ResumeFromGoalText(goal)
	if !ok {
		return legacyResearchSetup{}
	}
	if err != nil {
		slog.Warn("controller: resume legacy autoresearch task", "err", err)
		return legacyResearchSetup{explicit: true, blockReason: err.Error()}
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
	if report, err := m.store.ValidateTask(task.ID); err != nil {
		return "", err
	} else if !report.Valid {
		return "", errLegacyArchiveInvalid
	}
	goal := strings.TrimSpace(task.Spec.Goal)
	if goal == "" {
		return "", errLegacyArchiveMissingGoal
	}
	return goal, nil
}

var (
	errLegacyArchiveUnavailable = errString("legacy research archive is unavailable for this workspace")
	errLegacyArchiveInvalid     = errString("legacy research archive is invalid")
	errLegacyArchiveMissingGoal = errString("legacy research archive is missing goal text")
)

type errString string

func (e errString) Error() string { return string(e) }

func (c *Controller) prepareLegacyResearchTask(goal string) legacyResearchSetup {
	return c.legacyResearchArchive.prepare(goal)
}
