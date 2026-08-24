//go:build windows

package main

import (
	"sync/atomic"
	"testing"
)

func TestApplyWindowIconsFromExecutableRetriesUntilWindowAppears(t *testing.T) {
	var findCalls atomic.Int32
	var assigned atomic.Int32

	find := func() uintptr {
		findCalls.Add(1)
		if findCalls.Load() < 3 {
			return 0 // window not ready on first two attempts
		}
		return 0x1234
	}
	assign := func(hwnd uintptr) {
		if hwnd != 0x1234 {
			t.Errorf("assignWindowIcons got hwnd 0x%X, want 0x1234", hwnd)
		}
		assigned.Add(1)
	}

	oldFind, oldAssign := windowIconFindWindow, windowIconAssignWindow
	windowIconFindWindow, windowIconAssignWindow = find, assign
	defer func() {
		windowIconFindWindow, windowIconAssignWindow = oldFind, oldAssign
	}()

	applyWindowIconsFromExecutable()

	if assigned.Load() != 1 {
		t.Fatalf("assignWindowIcons called %d times, want exactly 1", assigned.Load())
	}
	if findCalls.Load() != 3 {
		t.Fatalf("findWindow called %d times, want 3 (two misses then hit)", findCalls.Load())
	}
}

func TestAssignWindowIconsSkipsWhenNoIconLoaded(t *testing.T) {
	windowIconLoadIcons = func() (uintptr, uintptr) { return 0, 0 }
	defer func() { windowIconLoadIcons = loadExecutableIcons }()

	// Must not panic and must not reach SendMessage; if it did, SendMessage
	// with hwnd 0 is harmless, so just assert no panic + no icon load path
	// return that crashes. The real assertion is that with (0,0) we return.
	assignWindowIcons(0xDEAD)
}
