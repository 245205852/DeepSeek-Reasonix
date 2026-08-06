package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCLICompletionCoversRunDispatch(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cli.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Dispatch lives in RunWithBuildInfo (Run is a thin BuildInfo wrapper).
	var dispatch *ast.SwitchStmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "RunWithBuildInfo" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switchStatement, ok := node.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			identifier, ok := switchStatement.Tag.(*ast.Ident)
			if ok && identifier.Name == "cmd" {
				dispatch = switchStatement
				return false
			}
			return true
		})
	}
	if dispatch == nil {
		t.Fatal("RunWithBuildInfo cmd dispatch switch not found")
	}

	root := cliCompletionRootSpec()
	for _, statement := range dispatch.Body.List {
		clause, ok := statement.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expression := range clause.List {
			literal, ok := expression.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			command, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !completionRegistryKnowsRootToken(&root, command) {
				t.Errorf("Run dispatch command %q is missing from the shell completion registry", command)
			}
		}
	}
}

func TestCLICompletionListsRootAndNestedCommands(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(cliCompletionValueKind) []string { return nil }

	got := cliCompletionCandidatesWithValues(root, 1, []string{"reasonix", "co"}, values)
	if want := []string{"config", "completion"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root completion = %v, want %v", got, want)
	}

	got = cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "mcp", "b"}, values)
	if want := []string{"browse"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mcp subcommand completion = %v, want %v", got, want)
	}

	got = cliCompletionCandidatesWithValues(root, 3, []string{"reasonix", "remote", "serve", "st"}, values)
	// "st" prefix matches start, stop, and status (registry order).
	if want := []string{"start", "stop", "status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote serve completion = %v, want %v", got, want)
	}
}

func TestCLICompletionListsCommandFlags(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(cliCompletionValueKind) []string { return nil }

	got := cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "run", "--m"}, values)
	for _, want := range []string{"--model", "--max-steps", "--metrics"} {
		if !containsCompletionValue(got, want) {
			t.Errorf("run flag completion missing %q: %v", want, got)
		}
	}

	got = cliCompletionCandidatesWithValues(root, 1, []string{"reasonix", "--d"}, values)
	for _, want := range []string{"--dir", "--dangerously-skip-permissions"} {
		if !containsCompletionValue(got, want) {
			t.Errorf("root flag completion missing %q: %v", want, got)
		}
	}

	got = cliCompletionCandidatesWithValues(root, 3, []string{"reasonix", "mcp", "add", "--h"}, values)
	for _, want := range []string{"--http", "--header", "--help"} {
		if !containsCompletionValue(got, want) {
			t.Errorf("mcp add flag completion missing %q: %v", want, got)
		}
	}
}

func TestCLICompletionUsesConfiguredModelsAndSessionIDs(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(kind cliCompletionValueKind) []string {
		switch kind {
		case cliCompletionModelValue:
			return []string{"deepseek/deepseek-chat", "mimo/mimo-v2"}
		case cliCompletionSessionValue:
			return []string{"alpha-session", "beta-session"}
		default:
			return nil
		}
	}

	got := cliCompletionCandidatesWithValues(root, 3, []string{"reasonix", "run", "--model", "deep"}, values)
	if want := []string{"deepseek/deepseek-chat"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model completion = %v, want %v", got, want)
	}

	got = cliCompletionCandidatesWithValues(root, 1, []string{"reasonix", "--model=mi"}, values)
	if want := []string{"--model=mimo/mimo-v2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inline model completion = %v, want %v", got, want)
	}

	got = cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "--resume", "b"}, values)
	if want := []string{"beta-session"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session completion = %v, want %v", got, want)
	}
}

func TestCompletionCommandPrintsShellScripts(t *testing.T) {
	isolateCLIConfigHome(t)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			out := captureStdout(t, func() {
				if code := Run([]string{"completion", shell}, "test-version"); code != 0 {
					t.Fatalf("completion %s exit code = %d", shell, code)
				}
			})
			if !strings.Contains(out, "reasonix completion __complete") {
				t.Fatalf("completion %s script does not route to the shared registry:\n%s", shell, out)
			}
			if shell == "fish" && strings.Contains(out, "complete -c reasonix -f ") {
				t.Fatal("fish completion must not use -f so path flags can fall back to files")
			}
		})
	}
}

func TestCLICompletionTaskNestedAndRunAblate(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(cliCompletionValueKind) []string { return nil }

	got := cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "task", "st"}, values)
	for _, want := range []string{"status", "stop"} {
		if !containsCompletionValue(got, want) {
			t.Fatalf("task prefix st missing %q: %v", want, got)
		}
	}
	got = cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "task", "mon"}, values)
	if !containsCompletionValue(got, "monitor") {
		t.Fatalf("task monitor missing: %v", got)
	}
	got = cliCompletionCandidatesWithValues(root, 3, []string{"reasonix", "task", "monitor", "st"}, values)
	if !containsCompletionValue(got, "status") || !containsCompletionValue(got, "stop") {
		t.Fatalf("task monitor st = %v", got)
	}
	got = cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "run", "--a"}, values)
	if !containsCompletionValue(got, "--ablate") {
		t.Fatalf("run --a missing --ablate: %v", got)
	}
}

func TestCLICompletionOptionalResumeThenFlag(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(kind cliCompletionValueKind) []string {
		if kind == cliCompletionSessionValue {
			return []string{"alpha-session"}
		}
		return nil
	}
	// --resume with no QUERY, then completing --m must offer --model (optional value).
	got := cliCompletionCandidatesWithValues(root, 2, []string{"reasonix", "--resume", "--m"}, values)
	if !containsCompletionValue(got, "--model") {
		t.Fatalf("optional --resume then --m = %v, want --model", got)
	}
}

func TestCLICompletionPathFlagReturnsEmptyForShellFallback(t *testing.T) {
	root := cliCompletionRootSpec()
	values := func(cliCompletionValueKind) []string { return []string{"should-not-appear"} }
	got := cliCompletionCandidatesWithValues(root, 3, []string{"reasonix", "run", "--dir", "do"}, values)
	if len(got) != 0 {
		t.Fatalf("path flag value candidates = %v, want empty for shell file fallback", got)
	}
}

func containsCompletionValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func completionRegistryKnowsRootToken(root *cliCompletionSpec, token string) bool {
	if strings.HasPrefix(token, "-") {
		flag, _ := cliCompletionLookupFlag(root, token)
		return flag != nil
	}
	return cliCompletionLookupSubcommand(root, token) != nil
}
