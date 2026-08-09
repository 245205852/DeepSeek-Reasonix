package main

import (
	"time"

	"reasonix/internal/control"
	"reasonix/internal/jobs"
)

// Interactive removal returns promptly; durable cleanup-pending markers let
// delayed cleanup safely own non-cooperative jobs after this grace expires.
const desktopSessionRemovalGrace = time.Second

func waitDestroyHandles(destroys []control.SessionDestroyHandle) bool {
	results := make(chan jobs.TeardownResult, len(destroys))
	waits := 0
	for _, destroy := range destroys {
		wait := destroy.Wait
		if destroy.WaitFor != nil {
			wait = func() jobs.TeardownResult { return destroy.WaitFor(desktopSessionRemovalGrace) }
		}
		if wait == nil {
			continue
		}
		waits++
		go func(wait func() jobs.TeardownResult) { results <- wait() }(wait)
	}
	if waits == 0 {
		return false
	}
	timer := time.NewTimer(desktopSessionRemovalGrace + 250*time.Millisecond)
	defer timer.Stop()
	timedOut := false
	for range waits {
		select {
		case result := <-results:
			timedOut = timedOut || result.HasTimedOut()
		case <-timer.C:
			return true
		}
	}
	return timedOut
}
