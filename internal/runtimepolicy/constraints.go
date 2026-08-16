package runtimepolicy

import (
	"regexp"
	"strings"

	"reasonix/internal/shellparse"
)

// Constraints are explicit user or host limits. They never encode task
// complexity, security keywords, or file counts.
type Constraints struct {
	ForbidMutation          bool
	ForbidTests             bool
	AllowedChecks           []string
	ForbidExternal          bool
	RequireFullVerification bool
	PlanModeReadOnly        bool
	Notes                   []string
}

// ParseConstraints accepts only explicit forbid/limit phrasing.
func ParseConstraints(instruction string) Constraints {
	var c Constraints
	lower := strings.ToLower(instruction)
	if matchesAny(lower, []string{
		"只分析", "只读", "不要修改", "别改", "不要改", "仅分析", "只看不改",
		"analyze only", "analysis only", "don't modify", "do not modify",
		"don't change", "do not change", "no changes", "read only", "read-only",
		"without modifying", "without changes", "don't edit", "do not edit",
		"复现但不修复", "只复现", "不要修复", "reproduce but don't fix",
		"reproduce only", "don't fix", "do not fix", "no fix",
	}) {
		c.ForbidMutation = true
		c.Notes = append(c.Notes, "user_forbid_mutation")
	}
	if matchesAny(lower, []string{
		"不要测试", "别跑测试", "不用测试", "跳过测试", "不要跑测试",
		"don't run tests", "do not run tests", "no tests", "skip tests",
		"without tests", "don't test", "do not test",
	}) {
		c.ForbidTests = true
		c.Notes = append(c.Notes, "user_forbid_tests")
	}
	if matchesAny(lower, []string{
		"完整验证", "全面验证", "闭环交付", "完整交付", "交付前检查", "验收闭环",
		"full verification", "complete verification", "verify everything",
		"closed-loop delivery", "deliver with verification",
	}) {
		c.RequireFullVerification = true
		c.Notes = append(c.Notes, "user_require_full_verification")
	}
	if cmds := parseAllowedChecks(instruction); len(cmds) > 0 {
		c.AllowedChecks = cmds
		c.Notes = append(c.Notes, "user_allowed_checks")
	}
	if matchesAny(lower, []string{
		"不要 push", "不要push", "别 push", "别push", "不要推送", "不要发布",
		"don't push", "do not push", "no push", "don't publish", "do not publish",
		"no publish", "don't deploy", "do not deploy",
	}) {
		c.ForbidExternal = true
		c.Notes = append(c.Notes, "user_forbid_external")
	}
	return c
}

// StripQuotedConstraints removes fenced and quoted spans so cited phrases
// cannot bind the host.
func StripQuotedConstraints(raw string) string {
	s := stripFences(raw)
	s = stripInlineCode(s)
	s = stripQuoted(s, '"', '"')
	s = stripQuoted(s, '“', '”')
	s = stripQuoted(s, '「', '」')
	return strings.TrimSpace(s)
}

func (c Constraints) AllowsMutation() bool {
	return !c.ForbidMutation && !c.PlanModeReadOnly
}

func (c Constraints) AllowsTests() bool { return !c.ForbidTests }

func (c Constraints) AllowsExternal() bool { return !c.ForbidExternal }

func (c Constraints) AllowsCommand(command string) bool {
	if !c.AllowsTests() {
		return false
	}
	command = strings.TrimSpace(command)
	if command == "" || len(c.AllowedChecks) == 0 {
		return true
	}
	for _, allowed := range c.AllowedChecks {
		if strings.EqualFold(strings.TrimSpace(allowed), command) {
			return true
		}
	}
	commandFields, malformed := shellparse.StaticFields(command)
	if malformed != "" || len(commandFields) == 0 {
		return false
	}
	for _, allowed := range c.AllowedChecks {
		allowedFields, malformed := shellparse.StaticFields(strings.TrimSpace(allowed))
		if malformed == "" && len(allowedFields) > 0 && hasFieldPrefix(commandFields, allowedFields) {
			return true
		}
	}
	return false
}

func parseAllowedChecks(instruction string) []string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)只跑\s+([^\n,，;；]+)`),
		regexp.MustCompile(`(?i)只运行\s+([^\n,，;；]+)`),
		regexp.MustCompile(`(?i)only\s+run\s+([^\n,;]+)`),
		regexp.MustCompile(`(?i)just\s+run\s+([^\n,;]+)`),
	}
	var out []string
	for _, re := range patterns {
		m := re.FindStringSubmatch(instruction)
		if len(m) < 2 {
			continue
		}
		cmd := strings.Trim(strings.TrimSpace(m[1]), "\"'`。.")
		if cmd != "" {
			out = append(out, cmd)
		}
	}
	return out
}

func matchesAny(lower string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func hasFieldPrefix(fields, prefix []string) bool {
	if len(prefix) > len(fields) {
		return false
	}
	for i := range prefix {
		if !strings.EqualFold(fields[i], prefix[i]) {
			return false
		}
	}
	return true
}

func stripFences(s string) string {
	var b strings.Builder
	inFence := false
	for line := range strings.SplitSeq(s, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func stripInlineCode(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '`' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripQuoted(s string, open, close rune) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if !in && r == open {
			in = true
			continue
		}
		if in && r == close {
			in = false
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}
