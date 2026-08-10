package config

import (
	"fmt"
	"strings"
)

// DesktopReasoningDisplayMode normalizes the desktop-only presentation mode.
func (c *Config) DesktopReasoningDisplayMode() string {
	raw := strings.ToLower(strings.TrimSpace(c.Desktop.ReasoningDisplayMode))
	switch raw {
	case "hidden", "summary", "auto":
		return raw
	}
	if raw != "" {
		return "summary"
	}
	if c.Desktop.ExpandThinking {
		return "auto"
	}
	return "summary"
}

// DesktopReasoningDisplayModeExplicit reports whether a valid new enum was stored.
func (c *Config) DesktopReasoningDisplayModeExplicit() bool {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.ReasoningDisplayMode)) {
	case "hidden", "summary", "auto":
		return true
	default:
		return false
	}
}

// SetDesktopReasoningDisplayMode writes the UI preference and its legacy alias.
func (c *Config) SetDesktopReasoningDisplayMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "hidden", "summary":
		c.Desktop.ReasoningDisplayMode = strings.ToLower(strings.TrimSpace(mode))
		c.Desktop.ExpandThinking = false
	case "auto":
		c.Desktop.ReasoningDisplayMode = "auto"
		c.Desktop.ExpandThinking = true
	default:
		return fmt.Errorf("reasoning display mode %q: must be hidden|summary|auto", mode)
	}
	return nil
}

// SetExpandThinking preserves the legacy desktop edit API.
func (c *Config) SetExpandThinking(on bool) error {
	if on {
		return c.SetDesktopReasoningDisplayMode("auto")
	}
	return c.SetDesktopReasoningDisplayMode("summary")
}

func renderDesktopReasoningDisplayMode(b *strings.Builder, c *Config) {
	legacyExpand := c.Desktop.ExpandThinking
	if strings.TrimSpace(c.Desktop.ReasoningDisplayMode) != "" && !c.DesktopReasoningDisplayModeExplicit() {
		// Unknown future/invalid enums read as summary. When another setting is
		// saved, keep that safe fallback stable after the invalid field is omitted.
		legacyExpand = false
	}
	fmt.Fprintf(b, "expand_thinking = %v   # desktop: legacy reasoning display alias; use reasoning_display_mode\n", legacyExpand)
	if c.DesktopReasoningDisplayModeExplicit() {
		fmt.Fprintf(b, "reasoning_display_mode = %q   # desktop: hidden|summary|auto reasoning presentation\n", c.DesktopReasoningDisplayMode())
	}
}
