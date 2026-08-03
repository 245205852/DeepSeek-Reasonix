package protocol

import (
	"testing"

	"reasonix/internal/rpcwire"
)

func TestClassifyOutboundControlVsData(t *testing.T) {
	cases := []struct {
		meta rpcwire.OutboundMeta
		want rpcwire.OutboundPriority
	}{
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodRemotePing)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodSessionSubmit)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodTurnCancel)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodSessionSubscribe)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameResponse, Method: string(MethodSessionSubscribe)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameError}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodBrokerCatalogChanged)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodBrokerStreamCancel)}, rpcwire.PriorityControl},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodBrokerStreamChunk)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodBrokerStreamEnd)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodSessionEvent)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodSessionResyncRequired)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameResponse, Method: string(MethodSessionHistory)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameResponse, Method: string(MethodFilePreview)}, rpcwire.PriorityData},
		{rpcwire.OutboundMeta{Kind: rpcwire.FrameRequest, Method: string(MethodBrokerStreamOpen)}, rpcwire.PriorityData},
	}
	for _, tc := range cases {
		if got := ClassifyOutbound(tc.meta); got != tc.want {
			t.Fatalf("%+v = %v, want %v", tc.meta, got, tc.want)
		}
	}
}

func TestChunkEndShareDataQueue(t *testing.T) {
	if ClassifyOutbound(rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodBrokerStreamChunk)}) != rpcwire.PriorityData {
		t.Fatal("chunk must be data")
	}
	if ClassifyOutbound(rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodBrokerStreamEnd)}) != rpcwire.PriorityData {
		t.Fatal("end must share data queue with chunk")
	}
	if ClassifyOutbound(rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodSessionEvent)}) !=
		ClassifyOutbound(rpcwire.OutboundMeta{Kind: rpcwire.FrameNotification, Method: string(MethodSessionResyncRequired)}) {
		t.Fatal("event and resync must share one queue")
	}
}
