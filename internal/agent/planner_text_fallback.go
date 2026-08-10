package agent

// Legacy text-plan fallback: reading a planner's prose for decisions a
// submitted plan states in a field. It retires as a unit once both plan
// producers submit structured plans.

import (
	"slices"
	"strings"

	"reasonix/internal/event"
)

// plannerApprovalPhrases is the fallback for planners that ignore the
// structured marker. Claims of past approval ("用户已批准", "already approved")
// are deliberately included: the planner cannot know host approval state, so a
// claimed approval is re-gated instead of trusted.
var plannerApprovalPhrases = []string{
	"是否批准",
	"等待用户批准",
	"等待您的批准",
	"待用户批准",
	"批准这个方案",
	"批准该方案",
	"批准此方案",
	"批准这个计划",
	"批准该计划",
	"批准此计划",
	"批准方案后",
	"批准计划后",
	"用户已批准",
	"用户已经批准",
	"已经获得批准",
	"approve this plan",
	"approve the plan",
	"approval before",
	"waiting for approval",
	"awaiting approval",
	"wait for user approval",
	"user approved",
	"already approved",
	"has approved",
}

func plannerPlanRequestsApproval(plan string) bool {
	lower := strings.ToLower(strings.TrimSpace(plan))
	if lower == "" {
		return false
	}
	if strings.ToLower(lastNonEmptyLine(lower)) == plannerRequiresApprovalMarker {
		return true
	}
	// Match per line so a nearby negation ("无需等待用户批准", "no need to wait
	// for approval") exempts only its own phrase, not the whole plan.
	for rawLine := range strings.SplitSeq(lower, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		for _, phrase := range plannerApprovalPhrases {
			before, _, ok := strings.Cut(line, phrase)
			if !ok {
				continue
			}
			if approvalMentionNegated(before) {
				continue
			}
			return true
		}
	}
	return false
}

// approvalMentionNegated reports whether the text just before a matched phrase
// negates it, so a plan that rules out an approval round ("无需等待用户批准")
// does not trigger one. Only the nearby prefix counts, and erring toward
// gating is fine: the failure mode is an extra prompt, never a silent run.
func approvalMentionNegated(prefix string) bool {
	const window = 30
	if len(prefix) > window {
		prefix = prefix[len(prefix)-window:]
	}
	for _, neg := range []string{"无需", "无须", "不需要", "不需", "不必", "不用", "no need", "not require", "not required", "without"} {
		if strings.Contains(prefix, neg) {
			return true
		}
	}
	return false
}

func plannerPlanRequestsUserDecision(plan string) (event.AskQuestion, bool) {
	trimmed := strings.TrimSpace(plan)
	if trimmed == "" || plannerPlanRequestsApproval(trimmed) {
		return event.AskQuestion{}, false
	}
	if q, ok := parsePlannerAskBlock(trimmed); ok {
		return q, true
	}
	lower := strings.ToLower(trimmed)
	// Directive asks and claimed user choices only; bare mentions ("用户选择",
	// "user confirmation") are absent so ordinary plan wording such as
	// "运行测试确认目标行为不变" cannot conjure an ask dialog.
	decisionPhrases := []string{
		"需要用户选择",
		"让用户选择",
		"请用户选择",
		"等待用户选择",
		"用户已选择",
		"用户已经选择",
		"请选择",
		"选哪个",
		"哪种方案",
		"哪个方案",
		"哪一个方案",
		"需要用户确认",
		"请用户确认",
		"等待用户确认",
		"需要用户提供",
		"请用户提供",
		"等待用户提供",
		"need user to choose",
		"ask the user to choose",
		"user should choose",
		"user chose",
		"user has chosen",
		"user already chose",
		"which option",
		"which approach",
		"which plan",
		"please choose",
		"please confirm",
		"needs user confirmation",
		"need the user to provide",
		"ask the user to provide",
	}
	hasDecisionPhrase := false
	for _, phrase := range decisionPhrases {
		if strings.Contains(lower, phrase) {
			hasDecisionPhrase = true
			break
		}
	}
	if !hasDecisionPhrase {
		return event.AskQuestion{}, false
	}
	return event.AskQuestion{
		ID:      "planner_user_decision",
		Header:  "Planner",
		Prompt:  plannerQuestionPrompt(trimmed),
		Options: plannerDecisionOptions(trimmed),
	}, true
}

func parsePlannerAskBlock(plan string) (event.AskQuestion, bool) {
	lower := strings.ToLower(plan)
	start := strings.Index(lower, plannerAskStartMarker)
	end := strings.Index(lower, plannerAskEndMarker)
	if start < 0 || end <= start {
		return event.AskQuestion{}, false
	}
	block := plan[start+len(plannerAskStartMarker) : end]
	var question string
	var options []event.AskOption
	for raw := range strings.SplitSeq(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			key, value, ok = strings.Cut(line, "：")
		}
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "question", "问题":
			question = value
		case "option", "选项":
			if value != "" && len(options) < 4 {
				options = append(options, event.AskOption{Label: truncateRunes(value, 72)})
			}
		}
	}
	if strings.TrimSpace(question) == "" {
		question = "Planner needs your decision before execution. Choose an option or type your own answer."
	}
	if len(options) < 2 {
		options = plannerDecisionOptions(plan)
	}
	return event.AskQuestion{
		ID:      "planner_user_decision",
		Header:  "Planner",
		Prompt:  truncateRunes(question, 280),
		Options: options,
	}, true
}

func plannerQuestionPrompt(plan string) string {
	lines := strings.Split(plan, "\n")
	for _, v := range slices.Backward(lines) {
		line := strings.TrimSpace(strings.Trim(v, "-* \t"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.ContainsAny(line, "？?") ||
			strings.Contains(lower, "请选择") ||
			strings.Contains(lower, "please choose") ||
			strings.Contains(lower, "please confirm") ||
			strings.Contains(lower, "请用户") ||
			strings.Contains(lower, "需要用户") {
			return truncateRunes(line, 280)
		}
	}
	return "Planner needs your decision before execution. Choose an option or type your own answer."
}

func plannerDecisionOptions(plan string) []event.AskOption {
	choices := extractPlannerDecisionOptions(plan)
	if len(choices) >= 2 {
		opts := make([]event.AskOption, 0, min(len(choices), 4))
		for _, choice := range choices {
			opts = append(opts, event.AskOption{Label: truncateRunes(choice, 72)})
			if len(opts) == 4 {
				break
			}
		}
		return opts
	}
	return []event.AskOption{
		{Label: "Type my answer", Description: "Use the custom answer row to provide the missing choice or information."},
		{Label: "Pause", Description: "Do not execute yet; I will reply in chat."},
	}
}

func extractPlannerDecisionOptions(plan string) []string {
	lines := strings.Split(plan, "\n")
	out := make([]string, 0, 4)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		candidate := ""
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(line, "方案") || strings.HasPrefix(line, "选项"):
			candidate = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(line, "方案"), "选项"), "一二三四五六七八九十1234567890.、:：)） \t"))
		case strings.HasPrefix(lower, "option ") || strings.HasPrefix(lower, "approach "):
			if idx := strings.IndexAny(line, ":：-—"); idx >= 0 && idx+1 < len(line) {
				candidate = strings.TrimSpace(line[idx+1:])
			}
		default:
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				prefix := strings.TrimRight(fields[0], ".)、:：")
				if len(prefix) == 1 && ((prefix[0] >= 'A' && prefix[0] <= 'D') || (prefix[0] >= 'a' && prefix[0] <= 'd')) {
					candidate = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
				}
			}
		}
		candidate = strings.TrimSpace(strings.Trim(candidate, "-—:： \t"))
		if candidate == "" || looksLikePlanStep(candidate) {
			continue
		}
		out = append(out, candidate)
		if len(out) == 4 {
			break
		}
	}
	return out
}

func looksLikePlanStep(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"read ", "edit ", "update ", "run ", "test ", "检查", "读取", "修改", "更新", "运行", "测试"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
