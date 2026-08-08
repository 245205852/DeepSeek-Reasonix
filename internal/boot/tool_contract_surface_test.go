package boot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestBootToolContractCoversProviderVisibleSurface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tokenMode string
	}{
		{name: "default", tokenMode: ""},
		{name: "economy", tokenMode: TokenModeEconomy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigHome(t)
			dir := robustTempDir(t)
			t.Chdir(dir)
			writeFile(t, dir, "reasonix.toml", `
default_model = "test-model"

[agent]
system_prompt = "BASE"

[[providers]]
name = "test-model"
kind = "boot-token-profile-test"
model = "x"
`)

			req, entries := captureTokenProfileSurface(t, tc.tokenMode)
			wantNames := defaultFullBootToolNames()
			if tc.tokenMode == TokenModeEconomy {
				wantNames = economyBootToolNames()
			}
			if got := toolSchemaNames(req.Tools); !reflect.DeepEqual(got, wantNames) {
				t.Fatalf("%s provider-visible tool surface changed\ngot  %v\nwant %v", tc.name, got, wantNames)
			}
			entryByName := make(map[string]tool.ContractEntry, len(entries))
			for _, entry := range entries {
				entryByName[entry.Name] = entry
			}
			if _, ok := entryByName["update_goal"]; !ok {
				t.Fatalf("static contract must retain contextual update_goal: %v", contractEntryNames(entries))
			}
			if len(entries) != len(req.Tools)+1 {
				t.Fatalf("contract entries = %d, provider tools = %d; want only contextual update_goal hidden\ncontract=%v\nprovider=%v", len(entries), len(req.Tools), contractEntryNames(entries), toolSchemaNames(req.Tools))
			}
			for _, s := range req.Tools {
				e, ok := entryByName[s.Name]
				if !ok {
					t.Fatalf("provider tool %q missing from static contract", s.Name)
				}
				if e.Description != strings.TrimSpace(s.Description) {
					t.Fatalf("%s description drift\ncontract=%q\nprovider=%q", e.Name, e.Description, s.Description)
				}
				if !json.Valid(e.Schema) {
					t.Fatalf("%s contract schema is invalid JSON: %s", e.Name, e.Schema)
				}
				if got := string(provider.CanonicalizeSchema(e.Schema)); got != string(e.Schema) {
					t.Fatalf("%s contract schema is not canonical", e.Name)
				}
				if string(e.Schema) != string(s.Parameters) {
					t.Fatalf("%s schema drift\ncontract=%s\nprovider=%s", e.Name, e.Schema, s.Parameters)
				}
			}
			readOnly := map[string]bool{}
			for _, e := range entries {
				readOnly[e.Name] = e.ReadOnly
			}
			for name, want := range map[string]bool{
				"bash":                false,
				"read_file":           true,
				"connect_tool_source": tc.tokenMode == TokenModeEconomy,
			} {
				got, ok := readOnly[name]
				if !ok {
					if name == "connect_tool_source" && tc.tokenMode != TokenModeEconomy {
						continue
					}
					t.Fatalf("contract missing %s; tools=%v", name, contractEntryNames(entries))
				}
				if got != want {
					t.Fatalf("%s ReadOnly = %v, want %v", name, got, want)
				}
			}
		})
	}
}
