package history

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"reasonix/internal/agent"
	"reasonix/internal/historycatalog"
	"reasonix/internal/provider"
	"reasonix/internal/retrieval"
)

type indexedCatalogManager struct {
	once      sync.Once
	mu        sync.RWMutex
	catalog   *historycatalog.Catalog
	roots     map[string]historycatalog.Root
	observers []func(historycatalog.Status, []string, string)
}

var processHistoryCatalog indexedCatalogManager

// RegisterCatalogRoots lets a host such as Desktop seed every saved project
// without constructing a controller. Opening and scanning remain asynchronous.
func RegisterCatalogRoots(roots []historycatalog.Root) { processHistoryCatalog.register(roots) }

// RegisterCatalogObserver subscribes a host to revision/progress changes. The
// callback payload contains roots and counters only, never query or content.
func RegisterCatalogObserver(observer func(historycatalog.Status, []string, string)) {
	if observer == nil {
		return
	}
	processHistoryCatalog.mu.Lock()
	processHistoryCatalog.observers = append(processHistoryCatalog.observers, observer)
	processHistoryCatalog.mu.Unlock()
}

// SharedCatalog returns the process projection when opening has completed.
// Nil means callers should return an explicit partial/opening response.
func SharedCatalog() *historycatalog.Catalog { return processHistoryCatalog.get() }

func FlushSharedCatalog(ctx context.Context) error {
	if catalog := processHistoryCatalog.get(); catalog != nil {
		return catalog.Flush(ctx)
	}
	return nil
}

func (m *indexedCatalogManager) register(roots []historycatalog.Root) {
	m.mu.Lock()
	if m.roots == nil {
		m.roots = map[string]historycatalog.Root{}
	}
	for _, root := range roots {
		if strings.TrimSpace(root.Path) != "" {
			root.Path = filepath.Clean(root.Path)
			m.roots[root.Path] = root
		}
	}
	catalog := m.catalog
	m.mu.Unlock()
	if catalog != nil {
		for _, root := range roots {
			catalog.RegisterRoot(root)
		}
	}
	m.once.Do(func() {
		go func() {
			catalog, err := historycatalog.Open(context.Background(), historycatalog.Options{OnRevision: processHistoryCatalog.publish})
			if err != nil {
				return
			}
			m.mu.Lock()
			m.catalog = catalog
			pending := make([]historycatalog.Root, 0, len(m.roots))
			for _, root := range m.roots {
				pending = append(pending, root)
			}
			m.mu.Unlock()
			for _, root := range pending {
				catalog.RegisterRoot(root)
			}
		}()
	})
}

func (m *indexedCatalogManager) publish(status historycatalog.Status, roots []string, reason string) {
	m.mu.RLock()
	observers := append([]func(historycatalog.Status, []string, string){}, m.observers...)
	m.mu.RUnlock()
	for _, observer := range observers {
		observer(status, append([]string{}, roots...), reason)
	}
}

func (m *indexedCatalogManager) get() *historycatalog.Catalog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.catalog
}

// PersistObserver returns the process-wide non-blocking projection sink. The
// authoritative save has already released its path/file locks before this is
// called by agent.Session.
func PersistObserver() agent.SessionPersistObserver { return historyPersistObserver{} }

type historyPersistObserver struct{}

func (historyPersistObserver) EnqueueSessionPersist(event agent.SessionPersistEvent) bool {
	catalog := processHistoryCatalog.get()
	if catalog == nil {
		return false
	}
	processHistoryCatalog.mu.RLock()
	defer processHistoryCatalog.mu.RUnlock()
	bestLength := -1
	var selected historycatalog.Root
	for _, root := range processHistoryCatalog.roots {
		if underRoot(event.Path, root.Path) && len(root.Path) > bestLength {
			selected = root
			bestLength = len(root.Path)
		}
	}
	if bestLength < 0 {
		return false
	}
	if event.Removed {
		go func() { _ = catalog.Purge(context.Background(), event.Path) }()
		return true
	}
	return catalog.EnqueuePersist(selected, event)
}

type IndexedSearcher struct {
	legacy *Searcher
	roots  []historycatalog.Root
}

func NewIndexedSearcher(opts Options) *IndexedSearcher {
	legacy := NewSearcher(opts)
	roots := []historycatalog.Root{}
	add := func(path, source, scope, workspace string, archive bool) {
		if strings.TrimSpace(path) == "" {
			return
		}
		roots = append(roots, historycatalog.Root{Path: path, Source: source, Scope: scope, WorkspaceRoot: workspace, Archive: archive})
	}
	add(opts.SessionDir, scopeProject, scopeProject, opts.SessionDir, false)
	add(subagentsDir(opts.SessionDir), scopeProject, scopeProject, opts.SessionDir, false)
	if filepath.Clean(opts.GlobalSessionDir) != filepath.Clean(opts.SessionDir) {
		add(opts.GlobalSessionDir, scopeGlobal, scopeGlobal, "", false)
		add(subagentsDir(opts.GlobalSessionDir), scopeGlobal, scopeGlobal, "", false)
	}
	add(opts.ArchiveDir, "archive", scopeGlobal, "", true)
	processHistoryCatalog.register(roots)
	return &IndexedSearcher{legacy: legacy, roots: roots}
}

func (s *IndexedSearcher) rootsFor(scope string) []string {
	out := []string{}
	for _, root := range s.roots {
		if scope == scopeProject && root.Scope != scopeProject {
			continue
		}
		out = append(out, root.Path)
	}
	return out
}

func (s *IndexedSearcher) Search(ctx context.Context, req SearchRequest) ([]Hit, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, contextError("query is required")
	}
	queryTerms, err := retrieval.QueryTerms(query)
	if err != nil {
		return nil, err
	}
	scope, err := normalizeScope(req.Scope)
	if err != nil {
		return nil, err
	}
	limit := clamp(req.Limit, defaultLimit, maxLimit)
	kinds, err := normalizeKinds(req.Kinds)
	if err != nil {
		return nil, err
	}
	catalog := processHistoryCatalog.get()
	if catalog == nil {
		return []Hit{}, nil
	}
	kindNames := make([]string, 0, len(kinds))
	for kind := range kinds {
		kindNames = append(kindNames, string(kind))
	}
	sort.Strings(kindNames)
	result, err := catalog.Search(ctx, historycatalog.SearchRequest{
		// Exact roots are the agent authority boundary. Catalog scope describes
		// desktop grouping and must not change the history tool's project meaning.
		Query: query,
		Kinds: kindNames, ToolName: strings.TrimSpace(req.ToolName), Limit: min(limit*4, historycatalog.MaxLimit),
		Roots: s.rootsFor(scope),
	})
	if err != nil {
		return nil, err
	}
	loaded := map[string][]provider.Message{}
	failed := map[string]bool{}
	hits := make([]Hit, 0, len(result.Items))
	for _, candidate := range result.Items {
		if failed[candidate.SessionPath] {
			continue
		}
		messages, ok := loaded[candidate.SessionPath]
		if !ok {
			state, known, identityErr := agent.SessionContentIdentity(candidate.SessionPath)
			if identityErr != nil || (known && state.DigestHex != candidate.ContentDigest) {
				failed[candidate.SessionPath] = true
				catalog.EnqueueExisting(context.Background(), candidate.SessionPath)
				continue
			}
			messages, err = loadMessages(candidate.SessionPath)
			if err != nil {
				failed[candidate.SessionPath] = true
				continue
			}
			loaded[candidate.SessionPath] = messages
		}
		text, ok := candidateText(messages, candidate)
		if !ok {
			catalog.EnqueueExisting(context.Background(), candidate.SessionPath)
			continue
		}
		hits = append(hits, Hit{Score: candidate.Score, SessionPath: candidate.SessionPath,
			SessionID: sessionID(candidate.SessionPath), Source: candidate.Source, MessageIndex: candidate.MessageIndex,
			Role: provider.Role(candidate.Role), Kind: Kind(candidate.Kind), ToolName: candidate.ToolName,
			Snippet: retrieval.MakeSnippet(text, query, queryTerms, maxSnippet)})
	}
	hits = retrieval.KeepTopRelativeScore(hits, scoreFloor, func(hit Hit) float64 { return hit.Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func candidateText(messages []provider.Message, candidate historycatalog.Candidate) (string, bool) {
	if candidate.MessageIndex < 0 || candidate.MessageIndex >= len(messages) {
		return "", false
	}
	msg := messages[candidate.MessageIndex]
	switch Kind(candidate.Kind) {
	case KindUserText:
		return stripComposePrefixes(msg.Content), msg.Role == provider.RoleUser
	case KindAssistantText:
		return msg.Content, msg.Role == provider.RoleAssistant
	case KindToolInput:
		if candidate.PartIndex < 0 || candidate.PartIndex >= len(msg.ToolCalls) {
			return "", false
		}
		call := msg.ToolCalls[candidate.PartIndex]
		return strings.TrimSpace(call.Name + " " + call.Arguments), true
	case KindToolError, KindToolOutput:
		return strings.TrimSpace(msg.Name + " " + msg.Content), msg.Role == provider.RoleTool
	default:
		return "", false
	}
}

func (s *IndexedSearcher) Around(ctx context.Context, req AroundRequest) ([]MessageContext, error) {
	return s.legacy.Around(ctx, req)
}

func (s *IndexedSearcher) IndexStatus() historycatalog.Status {
	if catalog := processHistoryCatalog.get(); catalog != nil {
		return catalog.Status()
	}
	return historycatalog.Status{State: "opening", Pending: 1}
}

func contextError(message string) error { return &indexedInputError{message: message} }

type indexedInputError struct{ message string }

func (e *indexedInputError) Error() string { return e.message }
