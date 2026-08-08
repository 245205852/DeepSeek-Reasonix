package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

const deepSeekOfficialBalanceURL = "https://api.deepseek.com/user/balance"

// MigrateLegacyDeepSeekProtocolUserConfig upgrades only unmodified legacy
// DeepSeek provider aliases in the user-global config. It deliberately edits
// the original TOML in place instead of rendering Config, so comments, future
// fields, and unrelated provider blocks survive byte-for-byte.
func MigrateLegacyDeepSeekProtocolUserConfig() (bool, error) {
	path := userConfigLoadPath()
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	return editLegacyDeepSeekProtocolFile(path, "", true)
}

// UpgradeDeepSeekProviderProtocol switches one official DeepSeek provider
// family to Anthropic Messages after an explicit user action. Passing the
// canonical name "deepseek" upgrades matching canonical/legacy alias blocks.
func UpgradeDeepSeekProviderProtocol(path, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("upgrade DeepSeek protocol: empty provider name")
	}
	return editLegacyDeepSeekProtocolFile(path, name, false)
}

// UpgradeDeepSeekProviderProtocolUserConfig applies the explicit upgrade to
// the active user-global source, including a legacy config location.
func UpgradeDeepSeekProviderProtocolUserConfig(name string) (bool, error) {
	return UpgradeDeepSeekProviderProtocol(userConfigLoadPath(), name)
}

// CanUpgradeDeepSeekProviderProtocol reports whether Settings may offer the
// explicit protocol upgrade. Custom transport/capability fields prevent the
// automatic migration but remain preserved when the user confirms this action.
func CanUpgradeDeepSeekProviderProtocol(p *ProviderEntry) bool {
	if p == nil || !strings.EqualFold(strings.TrimSpace(p.Kind), "openai") ||
		!isOfficialDeepSeekOpenAIEndpoint(p.BaseURL) ||
		strings.TrimSpace(p.APIKeyEnv) != "DEEPSEEK_API_KEY" {
		return false
	}
	models := p.ModelList()
	switch strings.TrimSpace(p.Name) {
	case "deepseek-flash":
		return len(models) == 1 && strings.TrimSpace(models[0]) == "deepseek-v4-flash"
	case "deepseek-pro":
		return len(models) == 1 && strings.TrimSpace(models[0]) == "deepseek-v4-pro"
	case "deepseek":
		if len(models) == 0 {
			return false
		}
		for _, model := range models {
			switch strings.TrimSpace(model) {
			case "deepseek-v4-flash", "deepseek-v4-pro":
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func editLegacyDeepSeekProtocolFile(path, target string, automatic bool) (bool, error) {
	unlock, err := LockConfigFileEdits(path)
	if err != nil {
		return false, err
	}
	defer unlock()

	resolved, exists, err := statConfigPath(path)
	if err != nil || !exists {
		return false, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false, err
	}
	raw, err := fileencoding.ReadFileUTF8(resolved)
	if err != nil {
		return false, err
	}
	next, changed, err := rewriteLegacyDeepSeekProtocol(string(raw), target, automatic)
	if err != nil || !changed {
		return changed, err
	}
	if err := fileutil.AtomicWriteFile(resolved, []byte(next), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func rewriteLegacyDeepSeekProtocol(raw, target string, automatic bool) (string, bool, error) {
	var decoded struct {
		Providers []ProviderEntry `toml:"providers"`
	}
	if _, err := toml.Decode(raw, &decoded); err != nil {
		return raw, false, err
	}
	var generic struct {
		Providers []map[string]any `toml:"providers"`
	}
	if _, err := toml.Decode(raw, &generic); err != nil {
		return raw, false, err
	}

	lines := strings.Split(raw, "\n")
	blocks := providerTOMLBlocks(lines)
	if len(blocks) != len(decoded.Providers) || len(generic.Providers) != len(decoded.Providers) {
		return raw, false, fmt.Errorf("upgrade DeepSeek protocol: could not map provider tables safely")
	}

	changed := false
	for i := range decoded.Providers {
		entry := &decoded.Providers[i]
		eligible := CanUpgradeDeepSeekProviderProtocol(entry)
		if automatic {
			eligible = eligible && isUnmodifiedLegacyDeepSeekProvider(*entry, generic.Providers[i])
		} else {
			eligible = eligible && deepSeekUpgradeTargetMatches(target, entry.Name)
		}
		if !eligible {
			continue
		}
		if err := rewriteDeepSeekProviderBlock(lines, blocks[i]); err != nil {
			return raw, false, err
		}
		changed = true
	}
	return strings.Join(lines, "\n"), changed, nil
}

func isUnmodifiedLegacyDeepSeekProvider(p ProviderEntry, raw map[string]any) bool {
	if p.Name != "deepseek-flash" && p.Name != "deepseek-pro" {
		return false
	}
	if !isExactDeepSeekOpenAIEndpoint(p.BaseURL) {
		return false
	}
	allowed := map[string]bool{
		"name": true, "kind": true, "base_url": true, "model": true,
		"api_key_env": true, "balance_url": true, "context_window": true,
		"price": true,
	}
	for key := range raw {
		if !allowed[key] {
			return false
		}
	}
	for _, required := range []string{"name", "kind", "base_url", "model", "api_key_env"} {
		if _, ok := raw[required]; !ok {
			return false
		}
	}
	if p.BalanceURL != "" && strings.TrimRight(strings.TrimSpace(p.BalanceURL), "/") != deepSeekOfficialBalanceURL {
		return false
	}
	if p.ContextWindow != 0 && p.ContextWindow != 1_000_000 {
		return false
	}
	return p.Price == nil || IsKnownDeepSeekOfficialPricing(p.Model, p.Price)
}

func deepSeekUpgradeTargetMatches(target, providerName string) bool {
	target = strings.TrimSpace(target)
	providerName = strings.TrimSpace(providerName)
	if target == providerName {
		return true
	}
	if CanonicalDesktopOfficialProviderName(target) != "deepseek" {
		return false
	}
	return CanonicalDesktopOfficialProviderName(providerName) == "deepseek"
}

func isExactDeepSeekOpenAIEndpoint(raw string) bool {
	path, ok := deepSeekOpenAIEndpointPath(raw)
	return ok && path == ""
}

func isOfficialDeepSeekOpenAIEndpoint(raw string) bool {
	path, ok := deepSeekOpenAIEndpointPath(raw)
	return ok && (path == "" || path == "/v1")
}

func deepSeekOpenAIEndpointPath(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") ||
		!strings.EqualFold(u.Hostname(), "api.deepseek.com") || u.Port() != "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(u.EscapedPath(), "/"), true
}

type providerTOMLBlock struct {
	start int
	end   int
}

func providerTOMLBlocks(lines []string) []providerTOMLBlock {
	headerLines := make([]int, 0)
	providerStarts := make([]int, 0)
	state := tomlOutside
	for i, line := range lines {
		if state != tomlOutside {
			state = advanceTOMLStringState(state, line)
			continue
		}
		if tomlSectionHeader(line) != "" {
			headerLines = append(headerLines, i)
			if isProviderArrayTableHeader(line) {
				providerStarts = append(providerStarts, i)
			}
		}
		state = advanceTOMLStringState(tomlOutside, line)
	}
	out := make([]providerTOMLBlock, 0, len(providerStarts))
	for _, start := range providerStarts {
		end := len(lines)
		for _, header := range headerLines {
			if header > start {
				end = header
				break
			}
		}
		out = append(out, providerTOMLBlock{start: start, end: end})
	}
	return out
}

func isProviderArrayTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	if comment := strings.Index(trimmed, "#"); comment >= 0 {
		trimmed = strings.TrimSpace(trimmed[:comment])
	}
	return trimmed == "[[providers]]"
}

func rewriteDeepSeekProviderBlock(lines []string, block providerTOMLBlock) error {
	kindLine, baseURLLine := -1, -1
	for i := block.start + 1; i < block.end; i++ {
		switch {
		case isTOMLKeyAssignment(lines[i], "kind"):
			kindLine = i
		case isTOMLKeyAssignment(lines[i], "base_url"):
			baseURLLine = i
		}
	}
	if kindLine < 0 || baseURLLine < 0 {
		return fmt.Errorf("upgrade DeepSeek protocol: provider table is missing kind or base_url")
	}
	lines[kindLine] = replaceTOMLStringAssignment(lines[kindLine], "anthropic")
	lines[baseURLLine] = replaceTOMLStringAssignment(lines[baseURLLine], deepSeekAnthropicBaseURL)
	return nil
}

func replaceTOMLStringAssignment(line, value string) string {
	carriageReturn := strings.HasSuffix(line, "\r")
	line = strings.TrimSuffix(line, "\r")
	equals := strings.IndexByte(line, '=')
	if equals < 0 {
		return line
	}
	rhs := line[equals+1:]
	leadingLen := len(rhs) - len(strings.TrimLeft(rhs, " \t"))
	leading := rhs[:leadingLen]
	suffix := ""
	if comment := tomlInlineCommentIndex(rhs); comment >= 0 {
		spaceStart := comment
		for spaceStart > 0 && (rhs[spaceStart-1] == ' ' || rhs[spaceStart-1] == '\t') {
			spaceStart--
		}
		suffix = rhs[spaceStart:]
	}
	next := line[:equals+1] + leading + strconv.Quote(value) + suffix
	if carriageReturn {
		next += "\r"
	}
	return next
}

func tomlInlineCommentIndex(value string) int {
	inBasic, inLiteral, escaped := false, false, false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if inBasic {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inBasic = false
			}
			continue
		}
		if inLiteral {
			if ch == '\'' {
				inLiteral = false
			}
			continue
		}
		switch ch {
		case '"':
			inBasic = true
		case '\'':
			inLiteral = true
		case '#':
			return i
		}
	}
	return -1
}
