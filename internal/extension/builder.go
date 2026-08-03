package extension

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// Activator binds live resources (MCP sidecars, watchers) to a freshly
// frozen snapshot. Stage 2 wires none; the seam exists so later stages plug
// in without touching the build pipeline. A nil *RuntimeSet result is
// treated as an empty set bound to the snapshot's generation.
type Activator func(ctx context.Context, snap *RuntimeSnapshot) (*RuntimeSet, error)

// Builder assembles a RuntimeSnapshot from registered contributors through a
// fixed pipeline: Discover → Parse → Validate → Resolve → Assemble → Freeze →
// Activate. The pipeline is linear by design — every contributor sees the
// same rules, and every consumer reads the same frozen result.
type Builder struct {
	contributors []Contributor
	generation   uint64
	systemPrompt string
	activator    Activator
}

// NewBuilder returns an empty builder with generation 0.
func NewBuilder() *Builder { return &Builder{} }

// AddContributor registers contributors. Registration order never influences
// the snapshot — the catalog, winner rules, and interceptor chains all sort
// on contribution data, not arrival order — so callers may register in any
// order.
func (b *Builder) AddContributor(contributors ...Contributor) *Builder {
	b.contributors = append(b.contributors, contributors...)
	return b
}

// WithGeneration sets the snapshot generation. Generations pair with
// RuntimeSet.CloseIfGeneration to keep stale cleanup from closing a newer
// runtime's resources.
func (b *Builder) WithGeneration(gen uint64) *Builder {
	b.generation = gen
	return b
}

// WithSystemPrompt sets the assembled system prompt text. It participates in
// CacheHash because it is part of the provider-visible request prefix.
func (b *Builder) WithSystemPrompt(prompt string) *Builder {
	b.systemPrompt = prompt
	return b
}

// WithActivator installs the activation seam. The default activator returns
// an empty RuntimeSet bound to the snapshot generation.
func (b *Builder) WithActivator(a Activator) *Builder {
	b.activator = a
	return b
}

// ValidationError reports one malformed contribution. Build collects all of
// them so a broken manifest surfaces every problem in one pass.
type ValidationError struct {
	Kind   ContributionKind
	ID     string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.ID == "" {
		return fmt.Sprintf("extension: invalid %s contribution: %s", e.Kind, e.Reason)
	}
	return fmt.Sprintf("extension: invalid %s %q: %s", e.Kind, e.ID, e.Reason)
}

// Build runs the full pipeline and returns the frozen snapshot plus its bound
// runtime resources. Any validation, conflict, or activation error aborts the
// build: publishing half-resolved state would let a losing contribution leak
// into the runtime.
func (b *Builder) Build(ctx context.Context) (*RuntimeSnapshot, *RuntimeSet, error) {
	raw, err := b.discover(ctx)
	if err != nil {
		return nil, nil, err
	}
	parsed := parseContributions(raw)
	if err := validateContributions(parsed); err != nil {
		return nil, nil, err
	}
	resolved, replacements, err := resolveContributions(parsed)
	if err != nil {
		return nil, nil, err
	}
	snap := b.assemble(resolved, replacements)
	runtimeSet, err := b.activate(ctx, snap)
	if err != nil {
		return nil, nil, err
	}
	return snap, runtimeSet, nil
}

// discover asks every contributor for its offerings and stamps the
// per-contributor registration sequence. An empty Origin defaults to the
// contributor name so conflict reports always have something meaningful to
// say.
func (b *Builder) discover(ctx context.Context) ([]Contribution, error) {
	var out []Contribution
	for _, c := range b.contributors {
		contribs, err := c.Contribute(ctx)
		if err != nil {
			return nil, fmt.Errorf("extension: contributor %q: %w", c.Name(), err)
		}
		for i, ct := range contribs {
			ct.Order = i
			if ct.Source.Origin == "" {
				ct.Source.Origin = c.Name()
			}
			out = append(out, ct)
		}
	}
	return out, nil
}

// parseContributions normalizes raw contributions. Stage 2 has no manifest
// decoding to do — adapters hand over typed payloads — so parsing is limited
// to ID hygiene; the stage exists so later manifest formats slot into the
// pipeline without reordering it.
func parseContributions(in []Contribution) []Contribution {
	out := make([]Contribution, len(in))
	for i, ct := range in {
		ct.ID = strings.TrimSpace(ct.ID)
		out[i] = ct
	}
	return out
}

// validateContributions rejects malformed contributions before any winner
// rules run, so resolution never has to guess what an invalid entry meant.
func validateContributions(cs []Contribution) error {
	var errs []error
	for _, ct := range cs {
		if !knownKind(ct.Kind) {
			errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "unknown kind"})
			continue
		}
		if ct.ID == "" {
			errs = append(errs, &ValidationError{Kind: ct.Kind, Reason: "empty ID"})
			continue
		}
		if strings.ContainsAny(ct.ID, " \t\n") {
			errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "ID contains whitespace"})
		}
		if !knownScope(ct.Source.Scope) {
			errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: fmt.Sprintf("unknown scope %q", ct.Source.Scope)})
		}
		switch ct.Kind {
		case KindTool:
			errs = append(errs, validateToolContribution(ct)...)
		case KindProvider:
			if _, _, ok := splitProviderRef(ct.ID); !ok {
				errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "provider ID must be a <name>/<model> ref"})
			}
		case KindInterceptor:
			if !knownInterceptorPoint(InterceptorPoint(ct.ID)) {
				errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "unknown interceptor point"})
			}
			if err := ValidatePriority(ct.Priority); err != nil {
				errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: err.Error()})
			}
		}
	}
	return errors.Join(errs...)
}

// validateToolContribution enforces the tool ID contract: lowercase names,
// the mcp__<server>__<tool> namespace for MCP-backed tools, and a payload the
// assembler can render into a provider schema.
func validateToolContribution(ct Contribution) []error {
	var errs []error
	if ct.ID != strings.ToLower(ct.ID) {
		errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "tool IDs must be lowercase"})
	}
	if strings.HasPrefix(ct.ID, tool.MCPNamePrefix) {
		if _, _, ok := tool.SplitMCPName(ct.ID); !ok {
			errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "malformed MCP tool name, want mcp__<server>__<tool>"})
		}
	} else if _, isMCP := ct.Payload.(tool.MCPMetadata); isMCP {
		// An MCP-backed tool outside the mcp__ namespace would collide with
		// built-in names and bypass MCP-specific policy checks.
		errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "MCP tool IDs must start with mcp__"})
	}
	if _, ok := toolSchemaOf(ct); !ok {
		errs = append(errs, &ValidationError{Kind: ct.Kind, ID: ct.ID, Reason: "payload must be tool.Tool, tool.ContractEntry, or provider.ToolSchema"})
	}
	return errs
}

// toolSchemaOf renders a tool contribution's payload into a provider schema.
// Parameters are canonicalized here — once — so every consumer, including
// CacheHash, sees identical bytes regardless of how the contributor marshaled
// them.
func toolSchemaOf(ct Contribution) (provider.ToolSchema, bool) {
	switch p := ct.Payload.(type) {
	case tool.Tool:
		return provider.ToolSchema{
			Name:        p.Name(),
			Description: p.Description(),
			Parameters:  provider.CanonicalizeSchema(p.Schema()),
		}, true
	case tool.ContractEntry:
		return provider.ToolSchema{
			Name:        p.Name,
			Description: p.Description,
			Parameters:  provider.CanonicalizeSchema(p.Schema),
		}, true
	case provider.ToolSchema:
		p.Parameters = provider.CanonicalizeSchema(p.Parameters)
		return p, true
	default:
		return provider.ToolSchema{}, false
	}
}

// resolveContributions applies the winner rules and returns the effective
// contribution set plus the replacement-slot owners.
//
// Shadowed kinds (tools, skills, commands, MCP servers, providers, prompts,
// themes, UI actions, strategies): the highest-tier contribution wins the
// canonical ID; distinct sources tied at that tier are a hard ConflictError —
// the kernel refuses to pick a winner the user didn't ask for. Duplicates
// from a single source collapse to the first registration, mirroring the
// first-root-wins behavior inside today's discovery passes.
//
// Hooks and interceptors are additive: every contribution survives and
// nothing ever conflicts.
//
// Replacement claims come from payloads implementing SlotClaimer; a second
// claimant for a slot is a hard SlotConflictError.
func resolveContributions(cs []Contribution) ([]Contribution, map[Slot]ContributionSource, error) {
	type key struct {
		kind ContributionKind
		id   string
	}
	groups := map[key][]Contribution{}
	var order []key
	for _, ct := range cs {
		k := key{ct.Kind, ct.ID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], ct)
	}

	var errs []error
	resolved := make([]Contribution, 0, len(cs))
	claims := NewReplaceClaims()
	for _, k := range order {
		group := groups[k]
		if additiveKind(k.kind) {
			resolved = append(resolved, group...)
		} else {
			winner, sources, conflicted := resolveGroup(group)
			if conflicted {
				errs = append(errs, &ConflictError{Kind: k.kind, ID: k.id, Sources: sources})
				continue
			}
			resolved = append(resolved, winner)
		}
	}
	// Claims are collected across all contributions, not just winners: a
	// losing contribution must not silently keep a slot it declared, because
	// slots replace runtime behavior regardless of catalog shadowing.
	for _, ct := range cs {
		claimer, ok := ct.Payload.(SlotClaimer)
		if !ok {
			continue
		}
		for _, slot := range claimer.ReplacementSlots() {
			if err := claims.Claim(slot, ct.Source); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	return resolved, claims.Claims(), nil
}

// resolveGroup picks the winning contribution for one (kind, id): the first
// registration among the highest-tier entries, plus whether the top tier is
// disputed between distinct sources.
func resolveGroup(group []Contribution) (winner Contribution, sources []ContributionSource, conflicted bool) {
	best := -1
	for _, ct := range group {
		if r := tierRank(ct.Source.Scope); r > best {
			best = r
		}
	}
	var top []Contribution
	for _, ct := range group {
		if tierRank(ct.Source.Scope) == best {
			top = append(top, ct)
		}
	}
	if sources, ok := conflictingSources(group); ok {
		return Contribution{}, sources, true
	}
	winner = top[0]
	for _, ct := range top[1:] {
		if ct.Order < winner.Order {
			winner = ct
		}
	}
	return winner, nil, false
}

// assemble freezes the resolved set into an immutable snapshot. Everything
// derivable is derived here — schemas rendered and sorted, chains grouped and
// ordered, hashes computed — so snapshot accessors stay trivial copies.
func (b *Builder) assemble(resolved []Contribution, replacements map[Slot]ContributionSource) *RuntimeSnapshot {
	catalog := NewCatalog()
	catalog.Add(resolved...)

	schemas := make([]provider.ToolSchema, 0)
	for _, ct := range catalog.ByKind(KindTool) {
		schema, ok := toolSchemaOf(ct)
		if ok {
			schemas = append(schemas, schema)
		}
	}
	schemas = normalizeToolSchemas(schemas)

	chains := map[InterceptorPoint][]Contribution{}
	for _, ct := range catalog.ByKind(KindInterceptor) {
		point := InterceptorPoint(ct.ID)
		chains[point] = append(chains[point], ct)
	}
	for point, chain := range chains {
		chains[point] = SortInterceptors(chain)
	}

	repl := make(map[Slot]ContributionSource, len(replacements))
	for slot, owner := range replacements {
		repl[slot] = owner
	}

	systemHash, toolsHash, cacheHash := computeCacheShape(b.systemPrompt, schemas)

	catalog.freeze()
	return &RuntimeSnapshot{
		generation:       b.generation,
		catalog:          catalog,
		systemPrompt:     b.systemPrompt,
		toolSchemas:      schemas,
		interceptorChain: chains,
		replacements:     repl,
		cacheHash:        cacheHash,
		systemHash:       systemHash,
		toolsHash:        toolsHash,
	}
}

// activate binds runtime resources through the configured Activator, or
// returns an empty set. The snapshot is already frozen at this point: an
// activator must observe, never mutate.
func (b *Builder) activate(ctx context.Context, snap *RuntimeSnapshot) (*RuntimeSet, error) {
	if b.activator == nil {
		return NewRuntimeSet(snap.Generation()), nil
	}
	runtimeSet, err := b.activator(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("extension: activate generation %d: %w", snap.Generation(), err)
	}
	if runtimeSet == nil {
		runtimeSet = NewRuntimeSet(snap.Generation())
	}
	return runtimeSet, nil
}
