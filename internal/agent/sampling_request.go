package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func normalizeContextEditing(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "native") {
		return "native"
	}
	return "local"
}

type contextEditingState struct {
	requestedContextEditing       string // normalized user intent: local or native
	contextEditing                string // effective mode after provider capability resolution
	contextEditingPolicyVersion   string // provider-declared native request-shape version
	contextEditingFallbackEmitted atomic.Bool
}

func newContextEditingState(mode string, prov provider.Provider) contextEditingState {
	requested, effective, version := resolveContextEditing(mode, prov)
	return contextEditingState{
		requestedContextEditing:     requested,
		contextEditing:              effective,
		contextEditingPolicyVersion: version,
	}
}

func resolveContextEditing(mode string, prov provider.Provider) (requested, effective, policyVersion string) {
	requested = normalizeContextEditing(mode)
	if requested != "native" {
		return requested, "local", ""
	}
	caps := provider.ContextEditingCapabilitiesOf(prov)
	policyVersion = strings.TrimSpace(caps.PolicyVersion)
	if !caps.NativeToolUseClear || policyVersion == "" {
		return requested, "local", ""
	}
	return requested, "native", policyVersion
}

func (a *Agent) contextEditingPolicy() *provider.ContextEditingPolicy {
	if a == nil || a.contextEditing != "native" {
		return nil
	}
	_, snip, _ := a.compactThresholds()
	return &provider.ContextEditingPolicy{
		Mode:                    "native",
		TriggerInputTokens:      snip,
		KeepToolUses:            3,
		ClearAtLeastInputTokens: max(4096, a.minimumMaintenanceSavingsTokens()),
		ClearToolInputs:         false,
	}
}

func (a *Agent) contextEditingLineageSuffix() string {
	policy := a.contextEditingPolicy()
	if policy == nil {
		return ""
	}
	return fmt.Sprintf("|context-editing-native-%s-t%d-k%d-m%d-i%t",
		a.contextEditingPolicyVersion,
		policy.TriggerInputTokens,
		policy.KeepToolUses,
		policy.ClearAtLeastInputTokens,
		policy.ClearToolInputs,
	)
}

func (a *Agent) emitContextEditingFallbackNotice() {
	if a == nil || a.requestedContextEditing != "native" || a.contextEditing == "native" ||
		a.usageSource != event.UsageSourceExecutor || !a.contextEditingFallbackEmitted.CompareAndSwap(false, true) {
		return
	}
	a.sink.Emit(event.Event{
		Kind:   event.Notice,
		Level:  event.LevelInfo,
		Code:   event.NoticeCodeContextEditingFallback,
		Text:   "Native context editing is unavailable for this provider; using local context maintenance.",
		Detail: "context_editing=native requires the official Anthropic endpoint; compatible gateways and other providers remain local",
		Source: a.usageSource,
	})
}

// samplingRequest is a once-prepared, frozen provider request for one model
// round. All stream retries replay this exact payload — no synthetic recovery
// messages, no schema reorder, no previous_response_id drift from failed attempts.
type samplingRequest struct {
	req provider.Request
}

// prepareSamplingRequest freezes one model-round request (preflight + interceptors).
func (a *Agent) prepareSamplingRequest(ctx context.Context) (samplingRequest, error) {
	a.emitContextEditingFallbackNotice()
	// CreatedAt is durable UI metadata, not model input. Strip it from the
	// transport copy so wall-clock differences never invalidate the provider's
	// prompt-cache prefix (and custom providers cannot accidentally send it).
	prepared, err := a.contextManager().Prepare(ctx, ContextPreparePolicy{Trigger: CompactionTriggerPressure})
	if err != nil {
		return samplingRequest{}, err
	}
	requestMessages := append([]provider.Message(nil), provider.ModelMessages(prepared.Messages)...)
	requestMessages = a.providerProjectionMessages(requestMessages)
	for i := range requestMessages {
		requestMessages[i].CreatedAt = 0
	}
	// context.prepare: extensions may rewrite the message copy feeding THIS
	// request. The session log is never touched — the replacement is
	// ephemeral, so the next request starts from the unmodified history and
	requestMessages, err = a.interceptContextPrepare(ctx, requestMessages)
	if err != nil {
		return samplingRequest{}, err
	}
	req := provider.Request{
		Messages:       requestMessages,
		Tools:          a.tools.Schemas(),
		MaxTokens:      a.maxOutputTokens,
		Temperature:    provider.OptionalTemperature(a.temperature),
		ResponseFormat: responseFormatFromRequest(ctx),
		EffortOverride: a.governorOverride(),
	}
	req.ContextEditing = a.contextEditingPolicy()
	// provider.request: the fully assembled request gets one last ruling
	// (revalidated by the payload registry) before it goes on the wire.
	req, err = a.interceptProviderRequest(ctx, req)
	if err != nil {
		return samplingRequest{}, err
	}
	// Enforce the shared-window invariant on the final extension-adjusted
	// payload. This keeps prompt + output inside the provider context window
	// without changing message bytes, tool order, or ordinary request defaults.
	if budget, clipped, budgetErr := a.effectiveOutputBudget(req); budgetErr != nil {
		return samplingRequest{}, budgetErr
	} else if clipped {
		req.MaxTokens = budget
	}
	shape := a.requestCalibrationShape(req)
	a.activeReqShape.Store(&shape)
	return samplingRequest{req: freezeProviderRequest(req)}, nil
}

// providerProjectionMessages applies provider-specific role compatibility to a
// request copy. Projection sidecars retain logical user-turn boundaries so
// explicit range compression can continue to resolve anchors across calls.
func (a *Agent) providerProjectionMessages(msgs []provider.Message) []provider.Message {
	if a != nil && a.strictAlternatingRoles {
		return coalesceProjectionUserRuns(msgs)
	}
	return msgs
}

// freezeProviderRequest deep-copies the provider-visible request surface so
// retries share identical messages, tools order, temperature, and format.
func freezeProviderRequest(req provider.Request) provider.Request {
	out := req
	if len(req.Messages) > 0 {
		out.Messages = append([]provider.Message(nil), req.Messages...)
		for i := range out.Messages {
			if len(out.Messages[i].ToolCalls) > 0 {
				out.Messages[i].ToolCalls = append([]provider.ToolCall(nil), out.Messages[i].ToolCalls...)
			}
			if len(out.Messages[i].Images) > 0 {
				out.Messages[i].Images = append([]string(nil), out.Messages[i].Images...)
			}
			if len(out.Messages[i].ResponsesItems) > 0 {
				items := make([]json.RawMessage, len(out.Messages[i].ResponsesItems))
				for j, item := range out.Messages[i].ResponsesItems {
					items[j] = append(json.RawMessage(nil), item...)
				}
				out.Messages[i].ResponsesItems = items
			}
		}
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]provider.ToolSchema, len(req.Tools))
		for i, schema := range req.Tools {
			out.Tools[i] = schema
			if len(schema.Parameters) > 0 {
				out.Tools[i].Parameters = append(json.RawMessage(nil), schema.Parameters...)
			}
		}
	}
	if req.Temperature != nil {
		t := *req.Temperature
		out.Temperature = &t
	}
	if req.ResponseFormat != nil {
		rf := *req.ResponseFormat
		out.ResponseFormat = &rf
	}
	if req.ContextEditing != nil {
		policy := *req.ContextEditing
		out.ContextEditing = &policy
	}
	return out
}
