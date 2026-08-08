package main

import (
	"bufio"
	"encoding/json"
	"os"
)

// roundsSplitAt counts a run's top-level tool rounds on each side of the
// first-correct instant, plus verification commands executed after it —
// the raw material of the "kept verifying a finished answer" diagnosis.
func roundsSplitAt(path string, cutoffUnixMs int64) (before, after, verifyAfter int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()
	inModel := true
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec trajectoryRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Event == nil || rec.Event.Tool == nil || rec.Event.Tool.ParentID != "" {
			continue
		}
		switch rec.Event.Kind {
		case "tool_dispatch":
			if inModel {
				inModel = false
				if rec.TS <= cutoffUnixMs {
					before++
				} else {
					after++
				}
			}
		case "tool_result":
			inModel = true
			if v := rec.Event.Tool.Execution; v != nil && rec.TS > cutoffUnixMs &&
				(v.Verification == "passed" || v.Verification == "failed") {
				verifyAfter++
			}
		}
	}
	return before, after, verifyAfter
}

// roundEnds returns the unix-ms end of each top-level tool round: the last
// tool_result before the next round's dispatch. The final answer segment has
// no entry — its end state is the run's final grade.
func roundEnds(path string) []int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ends []int64
	var lastResult int64
	inModel := true
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec trajectoryRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Event == nil || rec.Event.Tool == nil || rec.Event.Tool.ParentID != "" {
			continue
		}
		switch rec.Event.Kind {
		case "tool_dispatch":
			if inModel && lastResult > 0 {
				ends = append(ends, lastResult)
			}
			inModel = false
		case "tool_result":
			inModel = true
			lastResult = rec.TS
		}
	}
	if lastResult > 0 {
		ends = append(ends, lastResult)
	}
	return ends
}
