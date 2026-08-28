package main

import "sync/atomic"

// mcpAppsSandbox gates the Desktop Apps Host. The Apps double-iframe sandbox
// needs a loopback listener; if it cannot start (port exhaustion, sandboxed
// build), the host degrades to the interactive profile — MCP Core and text
// results stay available and no server is falsely told this client renders
// apps. Probing happens before the first shared host is acquired.
type mcpAppsSandbox struct {
	down atomic.Bool
}

// noteUnavailable records a permanent sandbox startup failure for this
// process. Later host acquisitions degrade to interactive-v1.
func (s *mcpAppsSandbox) noteUnavailable() { s.down.Store(true) }

// available reports whether the Apps sandbox can serve this session.
func (s *mcpAppsSandbox) available() bool { return !s.down.Load() }
