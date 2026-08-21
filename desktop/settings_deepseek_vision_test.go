package main

import (
	"reflect"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/provider/openai"
)

func TestProviderViewPinsOfficialDeepSeekVisionSKU(t *testing.T) {
	p := config.ProviderEntry{
		Name:         "deepseek",
		Kind:         "openai",
		BaseURL:      "https://api.deepseek.com",
		Models:       []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		VisionModels: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
	}
	view := providerViewFromEntry(p, true, true)
	if view.VisionCapability != "unsupported" {
		t.Fatalf("VisionCapability = %q, want unsupported", view.VisionCapability)
	}
	if len(view.VisionModels) != 0 {
		t.Fatalf("ProviderView must not advertise Flash/Pro as vision-capable: %v", view.VisionModels)
	}
	resolved := p
	resolved.Model = "deepseek-v4-pro"
	if config.EffectiveVision(&resolved) {
		t.Fatal("stale Flash/Pro vision metadata must not enable runtime image input")
	}

	p.Models = append(p.Models, openai.OfficialDeepSeekVisionModel)
	view = providerViewFromEntry(p, true, true)
	if !reflect.DeepEqual(view.VisionModels, []string{openai.OfficialDeepSeekVisionModel}) {
		t.Fatalf("ProviderView.VisionModels = %v, want pinned vision SKU", view.VisionModels)
	}
	resolved = p
	resolved.Model = openai.OfficialDeepSeekVisionModel
	if !config.EffectiveVision(&resolved) {
		t.Fatal("selecting the pinned official DeepSeek vision SKU must enable image input")
	}
}
