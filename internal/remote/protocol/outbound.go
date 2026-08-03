package protocol

import "reasonix/internal/rpcwire"

// ClassifyOutbound assigns Remote Workbench frames to the dual-priority
// scheduler. Classification is stable across Desktop client and Host router so
// control operations are not blocked by large stream or history payloads.
//
// Control queue:
//   - remote/initialize, remote/ping, remote/detach
//   - mutations (submit, steer, cancel, approval, ask, profile, goal, ...)
//   - broker cancel and catalog control notifications
//   - subscribe request and its snapshot response
//   - error responses
//
// Data queue:
//   - broker/stream/chunk and matching broker/stream/end
//   - session/event and session/resync_required
//   - large query responses (history, content, file, git, research findings)
//   - broker/stream/open request bodies
//
// Order constraints are preserved inside each queue (FIFO). chunk/end and
// event/resync share the data queue so they cannot reverse relative order.
func ClassifyOutbound(meta rpcwire.OutboundMeta) rpcwire.OutboundPriority {
	switch meta.Kind {
	case rpcwire.FrameError:
		return rpcwire.PriorityControl
	case rpcwire.FrameResponse:
		return classifyResponse(meta.Method)
	case rpcwire.FrameRequest, rpcwire.FrameNotification:
		return classifyMethod(meta.Method)
	default:
		return classifyMethod(meta.Method)
	}
}

func classifyMethod(method string) rpcwire.OutboundPriority {
	switch Method(method) {
	case MethodBrokerStreamChunk, MethodBrokerStreamEnd,
		MethodSessionEvent, MethodSessionResyncRequired,
		MethodBrokerStreamOpen:
		return rpcwire.PriorityData
	case MethodSessionHistory, MethodSessionContent,
		MethodFileList, MethodFileSearch, MethodFilePreview,
		MethodGitHistory, MethodGitCommitDetail,
		MethodResearchFindings, MethodResearchList,
		MethodWorkspaceChanges, MethodWorkspaceChangeDetail,
		MethodWorkspaceBrowse:
		// Large query requests share the data queue with their responses so a
		// flood of history/file work cannot starve ping/cancel on either peer.
		return rpcwire.PriorityData
	default:
		return rpcwire.PriorityControl
	}
}

func classifyResponse(method string) rpcwire.OutboundPriority {
	switch Method(method) {
	case MethodSessionHistory, MethodSessionContent,
		MethodFileList, MethodFileSearch, MethodFilePreview,
		MethodGitHistory, MethodGitCommitDetail,
		MethodResearchFindings, MethodResearchList,
		MethodWorkspaceChanges, MethodWorkspaceChangeDetail,
		MethodWorkspaceBrowse,
		MethodBrokerStreamOpen, MethodBrokerCatalog:
		return rpcwire.PriorityData
	case MethodSessionSubscribe:
		// Snapshot responses must stay on the control path so reconnect
		// recovery completes ahead of queued stream traffic.
		return rpcwire.PriorityControl
	default:
		return rpcwire.PriorityControl
	}
}

// RemoteWireOutbound returns the dual-queue scheduler options shared by
// Desktop client and Host router.
func RemoteWireOutbound() *rpcwire.OutboundSchedulerOptions {
	return rpcwire.RemoteOutboundScheduler(ClassifyOutbound)
}
