package config

import "fmt"

func normalizeOpenAIReasoningEffort(e *ProviderEntry, level string) (string, error) {
	if isMimoEntry(e) {
		switch level {
		case "none", "low", "medium", "high":
			return level, nil
		default:
			return "", fmt.Errorf("usage: /effort auto|none|low|medium|high")
		}
	}
	switch level {
	case "low", "medium", "high":
		return level, nil
	default:
		return "", fmt.Errorf("usage: /effort auto|low|medium|high")
	}
}

func normalizeKimiK3ReasoningEffort(level string) (string, error) {
	switch level {
	case "low", "high", "max":
		return level, nil
	default:
		return "", fmt.Errorf("usage: /effort auto|low|high|max")
	}
}
