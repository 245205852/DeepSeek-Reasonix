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
// It is set by the main wiring (cli.go) and defaults to an in-memory
// store for smoke-testing.  Tests override it with mock stores.
var taskStore taskmonitor.Store

// SetTaskStore replaces the Store used by the task subcommands.
func SetTaskStore(s taskmonitor.Store) { taskStore = s }

func taskCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix task <list|status|events> [flags]")
		return 2
	}
	store := taskStore
	if store == nil {
		store = taskmonitor.NewInMemoryStore()
	}
	switch args[0] {
	case "list":
		return taskListCmd(store, args[1:])
	case "status":
		return taskStatusCmd(store, args[1:])
	case "events":
		return taskEventsCmd(store, args[1:])
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
