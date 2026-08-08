package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateLegacyDeepSeekProtocolUserConfigPreservesTOMLAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	path := filepath.Join(home, "config.toml")
	raw := `# keep this user comment
config_version = 4
default_model = "deepseek-flash/deepseek-v4-flash"
future_top_level = "preserve-me"

[[providers]]
name        = "deepseek-flash"
kind        = "openai" # legacy wire
base_url    = "https://api.deepseek.com"
model       = "deepseek-v4-flash"
api_key_env = "DEEPSEEK_API_KEY"
balance_url = "https://api.deepseek.com/user/balance"
context_window = 1000000

[[providers]]
name        = "deepseek-pro"
kind        = "openai"
base_url    = "https://api.deepseek.com/"
model       = "deepseek-v4-pro"
api_key_env = "DEEPSEEK_API_KEY"

[[providers]]
name = "other"
kind = "openai"
base_url = "https://gateway.example/v1"
model = "other-model"
api_key_env = "OTHER_KEY"
future_provider_field = "untouched"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := MigrateLegacyDeepSeekProtocolUserConfig()
	if err != nil {
		t.Fatalf("MigrateLegacyDeepSeekProtocolUserConfig: %v", err)
	}
	if !changed {
		t.Fatal("legacy official providers were not migrated")
	}
	updatedBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := string(updatedBytes)
	if strings.Count(updated, `kind        = "anthropic"`) != 2 ||
		strings.Count(updated, `base_url    = "https://api.deepseek.com/anthropic"`) != 2 {
		t.Fatalf("migrated provider protocol mismatch:\n%s", updated)
	}
	for _, preserved := range []string{
		"# keep this user comment",
		`future_top_level = "preserve-me"`,
		`future_provider_field = "untouched"`,
		`base_url = "https://gateway.example/v1"`,
		`kind        = "anthropic" # legacy wire`,
	} {
		if !strings.Contains(updated, preserved) {
			t.Errorf("migration dropped %q:\n%s", preserved, updated)
		}
	}

	cfg, err := LoadForEditReadOnlyStrict(path)
	if err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	for _, name := range []string{"deepseek-flash", "deepseek-pro"} {
		entry, ok := cfg.Provider(name)
		if !ok {
			t.Fatalf("migrated provider %q missing", name)
		}
		if entry.Kind != "anthropic" || entry.BaseURL != deepSeekAnthropicBaseURL ||
			entry.Thinking != "enabled" || !EffectiveWebSearch(entry) ||
			len(entry.SupportedEfforts) == 0 || entry.DefaultEffort != "high" {
			t.Errorf("migrated provider %q capabilities = %+v", name, entry)
		}
	}

	beforeSecondRun := string(updatedBytes)
	changed, err = MigrateLegacyDeepSeekProtocolUserConfig()
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if changed {
		t.Fatal("second migration unexpectedly reported a change")
	}
	afterSecondRun, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSecondRun) != beforeSecondRun {
		t.Fatal("idempotent migration rewrote the config on its second run")
	}
}

func TestAutomaticDeepSeekProtocolMigrationKeepsCustomizedProviders(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{name: "proxy endpoint", extra: `base_url = "https://proxy.example/v1"`},
		{name: "custom headers", extra: `headers = { X-Route = "custom" }`},
		{name: "explicit model list", extra: `models = ["deepseek-v4-flash"]`},
		{name: "vision override", extra: `vision = true`},
		{name: "reasoning override", extra: `reasoning_protocol = "none"`},
		{name: "effort override", extra: `supported_efforts = ["high"]`},
		{name: "custom key", extra: `api_key_env = "MY_DEEPSEEK_KEY"`},
		{name: "unknown future field", extra: `future_capability = true`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL := `base_url = "https://api.deepseek.com"`
			apiKey := `api_key_env = "DEEPSEEK_API_KEY"`
			model := `model = "deepseek-v4-flash"`
			switch {
			case strings.HasPrefix(tt.extra, "base_url"):
				baseURL = tt.extra
			case strings.HasPrefix(tt.extra, "api_key_env"):
				apiKey = tt.extra
			case strings.HasPrefix(tt.extra, "models"):
				model = tt.extra
			}
			extra := tt.extra
			if tt.extra == baseURL || tt.extra == apiKey || tt.extra == model {
				extra = ""
			}
			raw := `[[providers]]
name = "deepseek-flash"
kind = "openai"
` + baseURL + "\n" + model + "\n" + apiKey + "\n" + extra + "\n"
			next, changed, err := rewriteLegacyDeepSeekProtocol(raw, "", true)
			if err != nil {
				t.Fatalf("rewriteLegacyDeepSeekProtocol: %v", err)
			}
			if changed || next != raw {
				t.Fatalf("customized provider was automatically migrated:\n%s", next)
			}
		})
	}
}

func TestManualDeepSeekProtocolUpgradePreservesCapabilitiesAndUnknownFields(t *testing.T) {
	raw := `[[providers]]
name = "deepseek-flash"
kind = 'openai'
base_url = 'https://api.deepseek.com'
model = "deepseek-v4-flash"
api_key_env = "DEEPSEEK_API_KEY"
vision = true
future_capability = "keep"

[[providers]]
name = "deepseek-pro"
kind = "openai"
base_url = "https://api.deepseek.com"
model = "deepseek-v4-pro"
api_key_env = "DEEPSEEK_API_KEY"
reasoning_protocol = "none"
`
	next, changed, err := rewriteLegacyDeepSeekProtocol(raw, "deepseek", false)
	if err != nil {
		t.Fatalf("rewriteLegacyDeepSeekProtocol: %v", err)
	}
	if !changed || strings.Count(next, `kind = "anthropic"`) != 2 ||
		strings.Count(next, `base_url = "https://api.deepseek.com/anthropic"`) != 2 {
		t.Fatalf("manual family upgrade mismatch:\n%s", next)
	}
	for _, preserved := range []string{
		`vision = true`,
		`future_capability = "keep"`,
		`reasoning_protocol = "none"`,
	} {
		if !strings.Contains(next, preserved) {
			t.Errorf("manual upgrade dropped %q:\n%s", preserved, next)
		}
	}
}

func TestCanUpgradeDeepSeekProviderProtocolRejectsProxyButAllowsExplicitUpgradeOfCustomization(t *testing.T) {
	base := ProviderEntry{
		Name: "deepseek-flash", Kind: "openai", BaseURL: "https://api.deepseek.com",
		Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
	}
	if !CanUpgradeDeepSeekProviderProtocol(&base) {
		t.Fatal("standard official provider should offer manual upgrade")
	}
	proxy := base
	proxy.BaseURL = "https://deepseek.example/v1"
	if CanUpgradeDeepSeekProviderProtocol(&proxy) {
		t.Fatal("proxy endpoint should not offer manual upgrade")
	}
	headers := base
	headers.Headers = map[string]string{"X-Route": "custom"}
	if !CanUpgradeDeepSeekProviderProtocol(&headers) {
		t.Fatal("custom headers should block automatic migration, not the explicit upgrade action")
	}
	versioned := base
	versioned.BaseURL = "https://api.deepseek.com/v1"
	if !CanUpgradeDeepSeekProviderProtocol(&versioned) {
		t.Fatal("the official /v1 compatibility address should offer the explicit upgrade action")
	}
	customKey := base
	customKey.APIKeyEnv = "MY_DEEPSEEK_KEY"
	if !CanUpgradeDeepSeekProviderProtocol(&customKey) {
		t.Fatal("an official provider with a custom key env should offer the explicit upgrade action")
	}
}

func TestNormalizeOfficialDeepSeekModelsDoesNotAddProToResponses(t *testing.T) {
	c := &Config{Providers: []ProviderEntry{{
		Name: "deepseek", Kind: "responses", BaseURL: "https://api.deepseek.com",
		Model: "deepseek-v4-flash",
	}}}

	normalizeOfficialDeepSeekModels(c)
	p, ok := c.Provider("deepseek")
	if !ok {
		t.Fatal("DeepSeek provider missing after normalization")
	}
	if !p.HasModel("deepseek-v4-flash") {
		t.Fatal("Responses provider lost its Flash model")
	}
	if p.HasModel("deepseek-v4-pro") {
		t.Fatalf("Responses normalization added unsupported Pro model: %+v", p.ModelList())
	}
}
