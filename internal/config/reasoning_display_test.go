package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestDesktopReasoningDisplayModeDefaultsAndLegacy(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		expand   bool
		want     string
		explicit bool
	}{
		{name: "missing defaults to summary", want: "summary"},
		{name: "legacy expand true maps to auto", expand: true, want: "auto"},
		{name: "explicit hidden wins over legacy", mode: "hidden", expand: true, want: "hidden", explicit: true},
		{name: "explicit summary wins over legacy", mode: "summary", expand: true, want: "summary", explicit: true},
		{name: "explicit auto", mode: "auto", want: "auto", explicit: true},
		{name: "unknown safely falls back", mode: "future-mode", want: "summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Desktop.ReasoningDisplayMode = tt.mode
			cfg.Desktop.ExpandThinking = tt.expand
			if got := cfg.DesktopReasoningDisplayMode(); got != tt.want {
				t.Fatalf("DesktopReasoningDisplayMode() = %q, want %q", got, tt.want)
			}
			if got := cfg.DesktopReasoningDisplayModeExplicit(); got != tt.explicit {
				t.Fatalf("DesktopReasoningDisplayModeExplicit() = %t, want %t", got, tt.explicit)
			}
		})
	}
}

func TestSetDesktopReasoningDisplayMode(t *testing.T) {
	tests := []struct {
		mode   string
		want   string
		expand bool
	}{
		{mode: "hidden", want: "hidden"},
		{mode: "summary", want: "summary"},
		{mode: "auto", want: "auto", expand: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := Default()
			if err := cfg.SetDesktopReasoningDisplayMode(tt.mode); err != nil {
				t.Fatalf("SetDesktopReasoningDisplayMode(%q): %v", tt.mode, err)
			}
			if got := cfg.DesktopReasoningDisplayMode(); got != tt.want {
				t.Fatalf("mode = %q, want %q", got, tt.want)
			}
			if cfg.Desktop.ExpandThinking != tt.expand {
				t.Fatalf("ExpandThinking = %t, want %t", cfg.Desktop.ExpandThinking, tt.expand)
			}
		})
	}

	cfg := Default()
	for _, invalid := range []string{"invalid", ""} {
		if err := cfg.SetDesktopReasoningDisplayMode(invalid); err == nil {
			t.Fatalf("invalid reasoning display mode %q unexpectedly accepted", invalid)
		}
	}
	if err := cfg.SetExpandThinking(true); err != nil {
		t.Fatalf("SetExpandThinking(true): %v", err)
	}
	if got := cfg.DesktopReasoningDisplayMode(); got != "auto" {
		t.Fatalf("legacy SetExpandThinking(true) mode = %q, want auto", got)
	}
}

func TestRenderTOMLIncludesExplicitReasoningDisplayMode(t *testing.T) {
	cfg := Default()
	if got := RenderTOML(cfg); strings.Contains(got, "reasoning_display_mode =") {
		t.Fatal("implicit default unexpectedly rendered reasoning_display_mode")
	}
	if err := cfg.SetDesktopReasoningDisplayMode("hidden"); err != nil {
		t.Fatalf("SetDesktopReasoningDisplayMode: %v", err)
	}
	rendered := RenderTOML(cfg)
	if !strings.Contains(rendered, `reasoning_display_mode = "hidden"`) {
		t.Fatalf("rendered config missing explicit reasoning display mode:\n%s", rendered)
	}
	if !strings.Contains(rendered, "expand_thinking = false") {
		t.Fatalf("rendered config missing legacy expand_thinking alias:\n%s", rendered)
	}
}

func TestDesktopReasoningDisplayModeTOMLRoundTrip(t *testing.T) {
	cfg := Default()
	if err := cfg.SetDesktopReasoningDisplayMode("auto"); err != nil {
		t.Fatalf("SetDesktopReasoningDisplayMode: %v", err)
	}
	var decoded Config
	if _, err := toml.Decode(RenderTOML(cfg), &decoded); err != nil {
		t.Fatalf("decode rendered config: %v", err)
	}
	if got := decoded.DesktopReasoningDisplayMode(); got != "auto" {
		t.Fatalf("round-tripped mode = %q, want auto", got)
	}
	if !decoded.DesktopReasoningDisplayModeExplicit() || !decoded.Desktop.ExpandThinking {
		t.Fatalf("round-tripped compatibility fields were not preserved: %+v", decoded.Desktop)
	}
}
