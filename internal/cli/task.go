package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"reasonix/internal/taskmonitor"
)

// taskStore is the taskmonitor.Store used by the task CLI commands.
// Tests override it with mock stores via SetTaskStore.  When nil, the
// CLI defaults to a FileStore backed by .reasonix/tasks under the
// project directory.
var taskStore taskmonitor.Store

// taskJobKiller is an optional JobKiller for stopping running tasks.
// It is injected by the main wiring or by cli.go when a controller is
// available (Desktop or running session).  When nil, kill is a no-op.
var taskJobKiller taskmonitor.JobKiller

// SetTaskStore replaces the Store used by the task subcommands.
func SetTaskStore(s taskmonitor.Store) { taskStore = s }

// SetTaskJobKiller sets the JobKiller for control subcommands.
// Called by the wiring when a controller with jobs.Manager is available.
func SetTaskJobKiller(k taskmonitor.JobKiller) { taskJobKiller = k }

func taskCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix task <list|status|events> [flags]")
		return 2
	}
	store := taskStore
	if store == nil {
		store = taskmonitor.NewFileStore(".reasonix/tasks")
	}
	switch args[0] {
	case "list":
		return taskListCmd(store, args[1:])
	case "status":
		return taskStatusCmd(store, args[1:])
	case "events":
		return taskEventsCmd(store, args[1:])
	case "stop":
		return taskStopCmd(store, args[1:])
	case "cancel":
		return taskCancelCmd(store, args[1:])
	case "resume":
		return taskResumeCmd(store, args[1:])
	case "open-session":
		return taskOpenSessionCmd(store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		return 2
	}
}

// --- list ---

func taskListCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task list requires --json")
		return 2
	}

	ctx := context.Background()
	tasks, err := store.ListTasks(ctx, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	output := struct {
		SchemaVersion int                        `json:"schema_version"`
		Tasks         []taskmonitor.TaskSnapshot `json:"tasks"`
	}{SchemaVersion: 1, Tasks: tasks}
	if tasks == nil {
		output.Tasks = []taskmonitor.TaskSnapshot{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// --- status ---

func taskStatusCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task status requires --json")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task status <id> --json [--dir DIR]")
		return 2
	}

	ctx := context.Background()
	snap, err := store.GetTask(ctx, *dir, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	output := struct {
		SchemaVersion int                       `json:"schema_version"`
		Task          *taskmonitor.TaskSnapshot `json:"task"`
	}{SchemaVersion: 1}
	if snap != nil {
		output.Task = snap
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// --- events ---

func taskEventsCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task events", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON array")
	jsonl := fs.Bool("jsonl", false, "output as JSONL stream")
	dir := fs.String("dir", "", "project directory scope")
	after := fs.Int("after", 0, "only events with Sequence > N")
	follow := fs.Bool("follow", false, "poll for new events until interrupted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut && !*jsonl {
		fmt.Fprintln(os.Stderr, "task events requires --json or --jsonl")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task events <id> --json|--jsonl [--dir DIR] [--after N] [--follow]")
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cursor := *after

	for {
		events, err := store.ListEvents(ctx, *dir, id, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return 0 // cancelled
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		// Find max sequence to update cursor
		for _, e := range events {
			if e.Sequence > cursor {
				cursor = e.Sequence
			}
		}

		if *jsonl {
			enc := json.NewEncoder(os.Stdout)
			for _, e := range events {
				if err := enc.Encode(e); err != nil {
					return 1
				}
			}
		} else {
			// --json: output as JSON array
			output := struct {
				SchemaVersion int                     `json:"schema_version"`
				TaskID        string                  `json:"task_id"`
				Events        []taskmonitor.TaskEvent `json:"events"`
			}{SchemaVersion: 1, TaskID: id, Events: events}
			if events == nil {
				output.Events = []taskmonitor.TaskEvent{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(output); err != nil {
				return 1
			}
		}

		if !*follow {
			break
		}
		// Check if task has reached a terminal state
		snap, _ := store.GetTask(ctx, *dir, id)
		if snap != nil && snap.State.Terminal() && len(events) == 0 {
			break
		}

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(500 * time.Millisecond):
		}
	}
	return 0
}

// --- control commands ---

func taskStopCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task stop", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	expectedVersion := fs.Uint64("expected-version", 0, "expected task version for CAS")
	reason := fs.String("reason", "", "reason for stopping")
	idemKey := fs.String("idempotency-key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task stop requires --json")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task stop <id> --expected-version N --json")
		return 2
	}

	ws, ok := store.(taskmonitor.WriteStore)
	if !ok {
		fmt.Fprintln(os.Stderr, "task stop: store does not support writes")
		return 1
	}
	cs := taskmonitor.NewControlService(ws)
	if taskJobKiller != nil {
		cs.SetJobKiller(taskJobKiller)
	}
	res, err := cs.StopTask(context.Background(), *dir, id, *expectedVersion, *reason, *idemKey)
	return outputControlResult(res, err)
}

func taskCancelCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task cancel", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	expectedVersion := fs.Uint64("expected-version", 0, "expected task version for CAS")
	reason := fs.String("reason", "", "reason for cancelling")
	idemKey := fs.String("idempotency-key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task cancel requires --json")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task cancel <id> --expected-version N --json")
		return 2
	}

	ws, ok := store.(taskmonitor.WriteStore)
	if !ok {
		fmt.Fprintln(os.Stderr, "task cancel: store does not support writes")
		return 1
	}
	cs := taskmonitor.NewControlService(ws)
	if taskJobKiller != nil {
		cs.SetJobKiller(taskJobKiller)
	}
	res, err := cs.CancelTask(context.Background(), *dir, id, *expectedVersion, *reason, *idemKey)
	return outputControlResult(res, err)
}

func taskResumeCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task resume", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	expectedVersion := fs.Uint64("expected-version", 0, "expected task version for CAS")
	idemKey := fs.String("idempotency-key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task resume requires --json")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task resume <id> --expected-version N --json")
		return 2
	}

	ws, ok := store.(taskmonitor.WriteStore)
	if !ok {
		fmt.Fprintln(os.Stderr, "task resume: store does not support writes")
		return 1
	}
	cs := taskmonitor.NewControlService(ws)
	if taskJobKiller != nil {
		cs.SetJobKiller(taskJobKiller)
	}
	res, err := cs.ResumeTask(context.Background(), *dir, id, *expectedVersion, *idemKey)
	return outputControlResult(res, err)
}

func taskOpenSessionCmd(store taskmonitor.Store, args []string) int {
	fs := flag.NewFlagSet("task open-session", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	dir := fs.String("dir", "", "project directory scope")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "task open-session requires --json")
		return 2
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: reasonix task open-session <id> --json")
		return 2
	}

	// open-session is read-only — use Store directly
	snap, err := store.GetTask(context.Background(), *dir, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res := taskmonitor.ControlResult{
		SchemaVersion: 1,
		Command:       "open_session",
		TaskID:        id,
	}
	if snap == nil {
		res.Error = &taskmonitor.CtrlError{Code: taskmonitor.ErrTaskNotFound, Message: "task not found"}
		return outputControlResult(res, nil)
	}
	res.SessionID = snap.SessionID
	res.State = snap.State
	res.Version = snap.Version
	res.Accepted = true
	return outputControlResult(res, nil)
}

func outputControlResult(res taskmonitor.ControlResult, err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !res.Accepted && !res.Idempotent {
		return 1
	}
	return 0
}
