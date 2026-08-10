package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/sessioncatalog"
)

type SessionCatalogStatus struct {
	State           string `json:"state"`
	Mode            string `json:"mode"`
	Revision        uint64 `json:"revision"`
	Indexed         int64  `json:"indexed"`
	Total           int64  `json:"total"`
	RepairPending   int64  `json:"repairPending"`
	LastError       string `json:"lastError,omitempty"`
	QuarantinedPath string `json:"quarantinedPath,omitempty"`
}

type ProjectTreeSnapshot struct {
	Revision     uint64               `json:"revision"`
	Projects     []ProjectNode        `json:"projects"`
	Catalog      SessionCatalogStatus `json:"catalog"`
	Indexed      int64                `json:"indexed"`
	Total        int64                `json:"total"`
	IndexingDone bool                 `json:"indexingDone"`
}

type ProjectTopicPageRequest struct {
	Scope         string `json:"scope"`
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Query         string `json:"query,omitempty"`
	TimeFilter    string `json:"timeFilter,omitempty"`
}

type ProjectTopicKey struct {
	Scope         string `json:"scope"`
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	TopicID       string `json:"topicId"`
}

type ProjectTopicPage struct {
	Items      []ProjectNode `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
	Revision   uint64        `json:"revision"`
}

type ProjectTreeChangedV2 struct {
	Revision uint64   `json:"revision"`
	Roots    []string `json:"roots"`
	Reason   string   `json:"reason"`
}

func sessionCatalogStatus(status sessioncatalog.Status) SessionCatalogStatus {
	return SessionCatalogStatus{
		State:           string(status.State),
		Mode:            string(status.Mode),
		Revision:        status.Revision,
		Indexed:         status.Indexed,
		Total:           status.Total,
		RepairPending:   status.RepairPending,
		LastError:       status.LastError,
		QuarantinedPath: status.QuarantinedPath,
	}
}

func (a *App) currentSessionCatalogStatus() SessionCatalogStatus {
	if a == nil {
		return SessionCatalogStatus{State: string(sessioncatalog.StateDegraded), Mode: string(sessioncatalog.ModeMemory)}
	}
	if a.catalogRebuilding.Load() {
		return SessionCatalogStatus{State: string(sessioncatalog.StateRebuilding)}
	}
	if catalog := a.sessionCatalog.Load(); catalog != nil {
		return sessionCatalogStatus(catalog.Status())
	}
	return SessionCatalogStatus{State: string(sessioncatalog.StateOpening)}
}

func (a *App) startSessionCatalog(rebuild bool) {
	if a == nil || a.shuttingDown.Load() {
		return
	}
	a.catalogLifecycleMu.Lock()
	if a.catalogCancel != nil {
		a.catalogLifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.bootContext())
	done := make(chan struct{})
	a.catalogCancel = cancel
	a.catalogDone = done
	a.catalogRebuilding.Store(rebuild)
	a.catalogLifecycleMu.Unlock()

	go func() {
		defer close(done)
		defer a.catalogRebuilding.Store(false)
		path := sessioncatalog.DefaultPath()
		targets := a.sessionCatalogTargets()
		if rebuild {
			if _, err := sessioncatalog.Rebuild(ctx, path, targets); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("desktop: rebuild session catalog", "err", err)
			}
		}
		catalog, err := sessioncatalog.Open(ctx, sessioncatalog.Options{
			Path: path,
			OnRevision: func(revision uint64, roots []string, reason string) {
				a.emitProjectTreeChangedV2(revision, roots, reason)
			},
		})
		if err != nil {
			slog.Warn("desktop: open session catalog", "err", err)
			return
		}
		if ctx.Err() != nil || a.shuttingDown.Load() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			_ = catalog.Close(closeCtx)
			closeCancel()
			return
		}
		a.sessionCatalog.Store(catalog)
		if err := a.syncSessionCatalogMetadata(ctx, catalog); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("desktop: sync session catalog metadata", "err", err)
		}
		select {
		case <-a.tabsRestoredSignal():
		case <-ctx.Done():
			return
		}
		a.indexRestoredSessionPaths(ctx, catalog)
		for _, target := range targets {
			if ctx.Err() != nil || a.shuttingDown.Load() {
				return
			}
			// Legacy assignment is deliberately background-only. It can scan and
			// repair old metadata, but no project-tree or controller request waits.
			if migrated := migrateLegacySessionsIntoGlobalTopics(target.Path); len(migrated) > 0 {
				_ = a.syncSessionCatalogMetadata(ctx, catalog)
			}
			if err := catalog.ReconcileDirectory(ctx, target); err != nil && !errors.Is(err, context.Canceled) {
				slog.Debug("desktop: reconcile session catalog directory", "dir", target.Path, "err", err)
			}
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := a.syncSessionCatalogMetadata(ctx, catalog); err != nil && !errors.Is(err, context.Canceled) {
					slog.Debug("desktop: refresh session catalog metadata", "err", err)
				}
				for _, target := range a.sessionCatalogTargets() {
					if migrated := migrateLegacySessionsIntoGlobalTopics(target.Path); len(migrated) > 0 {
						_ = a.syncSessionCatalogMetadata(ctx, catalog)
					}
					catalog.RequestReconcile(target)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (a *App) stopSessionCatalog(timeout time.Duration) {
	if a == nil {
		return
	}
	a.catalogLifecycleMu.Lock()
	cancel := a.catalogCancel
	done := a.catalogDone
	a.catalogCancel = nil
	a.catalogDone = nil
	a.catalogLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	catalog := a.sessionCatalog.Swap(nil)
	deadline := time.Now().Add(timeout)
	if catalog != nil {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		ctx, closeCancel := context.WithTimeout(context.Background(), remaining)
		_ = catalog.Close(ctx)
		closeCancel()
	}
	if done != nil {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
}

func (a *App) cancelAllTabBuilds() {
	if a == nil {
		return
	}
	a.mu.Lock()
	for _, tab := range a.tabs {
		a.supersedeTabBuildLocked(tab)
	}
	for _, tab := range a.detachedSessions {
		a.supersedeTabBuildLocked(tab)
	}
	a.mu.Unlock()
}

func (a *App) sessionCatalogTargets() []sessioncatalog.DirectoryTarget {
	f := loadProjectsFile()
	seen := map[string]bool{}
	out := []sessioncatalog.DirectoryTarget{}
	add := func(target sessioncatalog.DirectoryTarget) {
		target.Path = filepath.Clean(strings.TrimSpace(target.Path))
		if target.Path == "." || target.Path == "" || seen[target.Path] {
			return
		}
		seen[target.Path] = true
		out = append(out, target)
	}
	add(sessioncatalog.DirectoryTarget{Path: config.SessionDir(), Scope: "global"})
	add(sessioncatalog.DirectoryTarget{Path: desktopSessionDir(globalWorkspaceRoot()), Scope: "global"})
	for _, project := range f.Projects {
		add(sessioncatalog.DirectoryTarget{Path: desktopSessionDir(project.Root), Scope: "project", WorkspaceRoot: project.Root})
	}
	return out
}

func (a *App) indexRestoredSessionPaths(ctx context.Context, catalog *sessioncatalog.Catalog) {
	type restored struct {
		target sessioncatalog.DirectoryTarget
		path   string
	}
	a.mu.RLock()
	items := make([]restored, 0, len(a.tabs)+len(a.detachedSessions))
	collect := func(tab *WorkspaceTab) {
		if tab == nil || strings.TrimSpace(tab.SessionPath) == "" {
			return
		}
		items = append(items, restored{
			target: sessioncatalog.DirectoryTarget{Path: filepath.Dir(tab.SessionPath), Scope: tab.Scope, WorkspaceRoot: tab.WorkspaceRoot},
			path:   tab.SessionPath,
		})
	}
	for _, tab := range a.tabs {
		collect(tab)
	}
	for _, tab := range a.detachedSessions {
		collect(tab)
	}
	a.mu.RUnlock()
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		_ = catalog.IndexSessionPath(ctx, item.target, item.path)
	}
}

func (a *App) syncSessionCatalogMetadata(ctx context.Context, catalog *sessioncatalog.Catalog) error {
	f := loadProjectsFile()
	deleted := map[string]bool{}
	for _, topicID := range f.DeletedTopics {
		deleted[topicID] = true
	}
	projects := []sessioncatalog.ProjectRecord{{
		Scope: "global", Title: strings.TrimSpace(f.GlobalTitle), Color: normalizeProjectColor(f.GlobalColor),
	}}
	if projects[0].Title == "" {
		projects[0].Title = "Global"
	}
	topics := []sessioncatalog.TopicMetadata{}
	appendTopics := func(scope, root string, ids, pinnedIDs []string) {
		titles := loadTopicTitles(root)
		sources := loadTopicTitleSources(root)
		created := loadTopicCreatedAts(root)
		ordered := pinnedTopicIDs(orderedTopicIDs(ids, titles), pinnedIDs)
		for index, topicID := range ordered {
			if deleted[topicID] {
				continue
			}
			title := strings.TrimSpace(titles[topicID])
			if title == "" {
				title = defaultTopicTitle
			}
			topics = append(topics, sessioncatalog.TopicMetadata{
				Scope: scope, WorkspaceRoot: root, TopicID: topicID, Title: title,
				TitleSource: sources[topicID], Pinned: containsDesktopString(pinnedIDs, topicID),
				SortOrder: index, CreatedAt: topicCreatedAtForTree(created, topicID),
			})
		}
	}
	appendTopics("global", "", f.GlobalTopics, f.GlobalPinnedTopics)
	for index, project := range f.Projects {
		title := strings.TrimSpace(project.Title)
		if title == "" {
			title = workspaceName(project.Root)
		}
		projects = append(projects, sessioncatalog.ProjectRecord{
			Scope: "project", WorkspaceRoot: project.Root, Title: title, Color: project.Color,
			Pinned: containsDesktopString(f.PinnedProjects, project.Root), SortOrder: index,
		})
		appendTopics("project", project.Root, project.Topics, project.PinnedTopics)
	}
	return catalog.SyncMetadata(ctx, projects, topics)
}

func (a *App) emitProjectTreeChangedV2(revision uint64, roots []string, reason string) {
	if roots == nil {
		roots = []string{}
	}
	a.emitRuntimeEvent("project-tree:changed-v2", ProjectTreeChangedV2{Revision: revision, Roots: roots, Reason: reason})
	// One-release compatibility event. Its wrapper is catalog-only, so legacy
	// frontends refresh without reintroducing synchronous history I/O.
	a.emitRuntimeEvent("project-tree:changed")
}

func (a *App) requestSessionCatalogReconcile(dir string) {
	catalog := a.sessionCatalog.Load()
	if catalog == nil || a.shuttingDown.Load() || strings.TrimSpace(dir) == "" {
		return
	}
	clean := filepath.Clean(dir)
	target := sessioncatalog.DirectoryTarget{Path: clean, Scope: "global"}
	for _, candidate := range a.sessionCatalogTargets() {
		if sameDesktopPath(candidate.Path, clean) {
			target = candidate
			break
		}
	}
	go func() {
		if migrated := migrateLegacySessionsIntoGlobalTopics(target.Path); len(migrated) > 0 {
			ctx, cancel := context.WithTimeout(a.bootContext(), 5*time.Second)
			_ = a.syncSessionCatalogMetadata(ctx, catalog)
			cancel()
		}
		catalog.RequestReconcile(target)
	}()
}

func sessionDirectoryForPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
}

func (a *App) requestSessionCatalogPath(scope, workspaceRoot, path string) {
	catalog := a.sessionCatalog.Load()
	if catalog == nil || a.shuttingDown.Load() || strings.TrimSpace(path) == "" {
		return
	}
	catalog.RequestIndexSession(sessioncatalog.DirectoryTarget{
		Path: sessionDirectoryForPath(path), Scope: scope, WorkspaceRoot: workspaceRoot,
	}, path)
}

func (a *App) removeSessionCatalogPath(path, reason string) {
	catalog := a.sessionCatalog.Load()
	if catalog == nil || strings.TrimSpace(path) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 150*time.Millisecond)
	defer cancel()
	if err := catalog.RemoveSession(ctx, path, reason); err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("desktop: remove session catalog row", "err", err)
	}
}

func (a *App) requestSessionCatalogMetadataSync() {
	catalog := a.sessionCatalog.Load()
	if catalog == nil || a.shuttingDown.Load() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.bootContext(), 5*time.Second)
		defer cancel()
		_ = a.syncSessionCatalogMetadata(ctx, catalog)
	}()
}

func (a *App) GetProjectTreeSnapshot() ProjectTreeSnapshot {
	f := loadProjectsFile()
	projects := []ProjectNode{}
	if strings.TrimSpace(f.GlobalTitle) != "" || len(f.GlobalTopics) > 0 || len(f.Projects) == 0 {
		label := strings.TrimSpace(f.GlobalTitle)
		if label == "" {
			label = "Global"
		}
		projects = append(projects, ProjectNode{
			Key: "global_folder", Kind: "global_folder", Label: label,
			Root: globalWorkspaceRoot(), ProjectColor: normalizeProjectColor(f.GlobalColor),
			Children: []ProjectNode{},
		})
	}
	for _, project := range f.Projects {
		label := strings.TrimSpace(project.Title)
		if label == "" {
			label = workspaceName(project.Root)
		}
		projects = append(projects, ProjectNode{
			Key: "project_" + project.Root, Kind: "project", Label: label,
			Root: project.Root, ProjectColor: project.Color,
			Pinned:   containsDesktopString(f.PinnedProjects, project.Root),
			Children: []ProjectNode{},
		})
	}
	projects = applyPinnedProjectOrder(applyProjectTreeOrder(projects, f.SidebarOrder), f.PinnedProjects)
	status := a.currentSessionCatalogStatus()
	return ProjectTreeSnapshot{
		Revision: status.Revision, Projects: projects, Catalog: status,
		Indexed: status.Indexed, Total: status.Total,
		IndexingDone: status.State == string(sessioncatalog.StateReady) && status.RepairPending == 0,
	}
}

type catalogRuntimeSnapshot struct {
	scope         string
	workspaceRoot string
	topicID       string
	sessionPath   string
	activity      string
	topicTitle    string
	ctrl          control.SessionAPI
	open          bool
}

type catalogRuntimeOverlay struct {
	open    bool
	running bool
	status  string
}

func catalogRuntimeStatus(activity string, runtimeStatus control.RuntimeStatus) string {
	status := normalizeTopicStatus(activity)
	if runtimeStatus.PendingPrompt {
		return topicStatusWaitingConfirmation
	}
	if runtimeStatus.Running {
		if status == "" || status == topicStatusError || status == topicStatusPaused {
			return topicStatusThinking
		}
		return status
	}
	if runtimeStatus.BackgroundJobs > 0 {
		return topicStatusBackgroundJob
	}
	if status == topicStatusError || status == topicStatusPaused {
		return status
	}
	return status
}

func (a *App) catalogRuntimeOverlays() (map[string]catalogRuntimeOverlay, map[string]catalogRuntimeOverlay) {
	a.mu.RLock()
	snapshots := make([]catalogRuntimeSnapshot, 0, len(a.tabs)+len(a.detachedSessions))
	collect := func(tab *WorkspaceTab, open bool) {
		if tab == nil || strings.TrimSpace(tab.TopicID) == "" {
			return
		}
		snapshots = append(snapshots, catalogRuntimeSnapshot{
			scope: tab.Scope, workspaceRoot: tab.WorkspaceRoot, topicID: tab.TopicID,
			sessionPath: tab.SessionPath, activity: tab.ActivityStatus, topicTitle: tab.TopicTitle,
			ctrl: tab.Ctrl, open: open,
		})
	}
	for _, tab := range a.tabs {
		collect(tab, true)
	}
	for _, tab := range a.detachedSessions {
		collect(tab, false)
	}
	a.mu.RUnlock()
	topics := map[string]catalogRuntimeOverlay{}
	sessions := map[string]catalogRuntimeOverlay{}
	for _, snap := range snapshots {
		runtimeStatus := control.RuntimeStatus{}
		path := strings.TrimSpace(snap.sessionPath)
		if snap.ctrl != nil {
			runtimeStatus = snap.ctrl.RuntimeStatus()
			if path == "" {
				path = snap.ctrl.SessionPath()
			}
		}
		status := catalogRuntimeStatus(snap.activity, runtimeStatus)
		running := status != "" || runtimeStatus.Running || runtimeStatus.PendingPrompt || runtimeStatus.BackgroundJobs > 0
		overlay := catalogRuntimeOverlay{open: snap.open, running: running, status: status}
		key := topicSummaryKey(snap.scope, snap.workspaceRoot, snap.topicID)
		current := topics[key]
		current.open = current.open || overlay.open
		current.running = current.running || overlay.running
		if current.status == "" {
			current.status = overlay.status
		}
		topics[key] = current
		if path != "" {
			sessions[sessionRuntimeKey(path)] = overlay
		}
	}
	return topics, sessions
}

func (a *App) metadataProjectTopics(scope, workspaceRoot string) []ProjectNode {
	f := loadProjectsFile()
	deleted := map[string]bool{}
	for _, topicID := range f.DeletedTopics {
		deleted[topicID] = true
	}
	ids := f.GlobalTopics
	pinnedIDs := f.GlobalPinnedTopics
	titleRoot := ""
	projectColor := normalizeProjectColor(f.GlobalColor)
	if scope == "project" {
		ids = nil
		pinnedIDs = nil
		titleRoot = workspaceRoot
		for _, project := range f.Projects {
			if sameProjectRoot(project.Root, workspaceRoot) {
				ids = project.Topics
				pinnedIDs = project.PinnedTopics
				projectColor = project.Color
				break
			}
		}
	}
	titles := loadTopicTitles(titleRoot)
	sources := loadTopicTitleSources(titleRoot)
	created := loadTopicCreatedAts(titleRoot)
	topicOverlays, _ := a.catalogRuntimeOverlays()
	runtimeNodes := a.runtimeOnlyProjectTopics(scope, workspaceRoot)
	runtimeByTopic := map[string]ProjectNode{}
	for _, node := range runtimeNodes {
		runtimeByTopic[node.TopicID] = node
	}
	out := []ProjectNode{}
	seen := map[string]bool{}
	for _, topicID := range pinnedTopicIDs(orderedTopicIDs(ids, titles), pinnedIDs) {
		if deleted[topicID] {
			continue
		}
		seen[topicID] = true
		title := strings.TrimSpace(titles[topicID])
		if title == "" {
			title = defaultTopicTitle
		}
		kind := "topic"
		if scope != "project" {
			kind = "global_topic"
		}
		overlay := topicOverlays[topicSummaryKey(scope, workspaceRoot, topicID)]
		node := ProjectNode{
			Key: kind + "_" + topicID, Kind: kind,
			Label: a.localizedTopicTitle(title, sources[topicID]), Root: workspaceRoot,
			TopicID: topicID, ProjectColor: projectColor,
			CreatedAt: topicCreatedAtForTree(created, topicID), Pinned: containsDesktopString(pinnedIDs, topicID),
			Open: overlay.open, Running: overlay.running, Status: overlay.status,
			TurnsState: string(sessioncatalog.TurnsUnknown), Health: string(sessioncatalog.HealthOK),
			Children: []ProjectNode{},
		}
		if runtimeNode, ok := runtimeByTopic[topicID]; ok {
			node.Open = runtimeNode.Open
			node.Running = runtimeNode.Running
			node.Status = runtimeNode.Status
			node.Children = runtimeNode.Children
		}
		out = append(out, node)
	}
	for _, runtimeNode := range runtimeNodes {
		if seen[runtimeNode.TopicID] || deleted[runtimeNode.TopicID] {
			continue
		}
		out = append(out, runtimeNode)
	}
	return out
}

func (a *App) runtimeOnlyProjectTopics(scope, workspaceRoot string) []ProjectNode {
	a.mu.RLock()
	snapshots := []catalogRuntimeSnapshot{}
	collect := func(tab *WorkspaceTab, open bool) {
		if tab == nil || strings.TrimSpace(tab.TopicID) == "" {
			return
		}
		if scope == "project" {
			if tab.Scope != "project" || !sameProjectRoot(tab.WorkspaceRoot, workspaceRoot) {
				return
			}
		} else if tab.Scope == "project" {
			return
		}
		snapshots = append(snapshots, catalogRuntimeSnapshot{
			scope: tab.Scope, workspaceRoot: tab.WorkspaceRoot, topicID: tab.TopicID,
			sessionPath: tab.SessionPath, activity: tab.ActivityStatus,
			topicTitle: tab.TopicTitle, ctrl: tab.Ctrl, open: open,
		})
	}
	for _, tab := range a.tabs {
		collect(tab, true)
	}
	for _, tab := range a.detachedSessions {
		collect(tab, false)
	}
	a.mu.RUnlock()
	byTopic := map[string][]catalogRuntimeSnapshot{}
	for _, snapshot := range snapshots {
		if snapshot.sessionPath == "" && snapshot.ctrl != nil {
			snapshot.sessionPath = snapshot.ctrl.SessionPath()
		}
		byTopic[snapshot.topicID] = append(byTopic[snapshot.topicID], snapshot)
	}
	topicIDs := make([]string, 0, len(byTopic))
	for topicID := range byTopic {
		topicIDs = append(topicIDs, topicID)
	}
	sort.Strings(topicIDs)
	out := []ProjectNode{}
	for _, topicID := range topicIDs {
		sessions := byTopic[topicID]
		kind := "topic"
		sessionKind := "session"
		if scope != "project" {
			kind = "global_topic"
			sessionKind = "global_session"
		}
		label := defaultTopicTitle
		if strings.TrimSpace(sessions[0].topicTitle) != "" {
			label = sessions[0].topicTitle
		}
		node := ProjectNode{
			Key: kind + "_" + topicID, Kind: kind, Label: label,
			Root: workspaceRoot, TopicID: topicID, TurnsState: string(sessioncatalog.TurnsUnknown),
			Health: string(sessioncatalog.HealthOK), Children: []ProjectNode{},
		}
		for _, session := range sessions {
			runtimeStatus := control.RuntimeStatus{}
			if session.ctrl != nil {
				runtimeStatus = session.ctrl.RuntimeStatus()
			}
			status := catalogRuntimeStatus(session.activity, runtimeStatus)
			running := status != "" || runtimeStatus.Running || runtimeStatus.PendingPrompt || runtimeStatus.BackgroundJobs > 0
			if len(sessions) == 1 {
				node.Open = session.open
				node.Running = running
				node.Status = status
				continue
			}
			path := strings.TrimSpace(session.sessionPath)
			sessionLabel := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if sessionLabel == "" || sessionLabel == "." {
				sessionLabel = label
			}
			node.Children = append(node.Children, ProjectNode{
				Key: projectSessionNodeKey(scope, path), Kind: sessionKind, Label: sessionLabel,
				Root: workspaceRoot, TopicID: topicID, SessionPath: path,
				Open: session.open, Running: running, Status: status,
				TurnsState: string(sessioncatalog.TurnsUnknown), Health: string(sessioncatalog.HealthOK),
				Children: []ProjectNode{},
			})
		}
		out = append(out, node)
	}
	return out
}

func (a *App) metadataTopicPage(req ProjectTopicPageRequest) ProjectTopicPage {
	items := a.metadataProjectTopics(req.Scope, req.WorkspaceRoot)
	query := strings.ToLower(strings.TrimSpace(req.Query))
	if query != "" {
		filtered := items[:0]
		for _, item := range items {
			if strings.Contains(strings.ToLower(item.Label), query) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	start := 0
	if strings.HasPrefix(req.Cursor, "meta:") {
		lastID := strings.TrimPrefix(req.Cursor, "meta:")
		for index, item := range items {
			if item.TopicID == lastID {
				start = index + 1
				break
			}
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = sessioncatalog.DefaultLimit
	}
	if limit > sessioncatalog.MaxLimit {
		limit = sessioncatalog.MaxLimit
	}
	end := min(start+limit, len(items))
	page := ProjectTopicPage{Items: append([]ProjectNode(nil), items[start:end]...)}
	if end < len(items) && end > start {
		page.NextCursor = "meta:" + items[end-1].TopicID
	}
	return page
}

func (a *App) projectNodeFromCatalogTopic(topic sessioncatalog.TopicRecord, topicOverlays, sessionOverlays map[string]catalogRuntimeOverlay) ProjectNode {
	kind := "topic"
	if topic.Scope == "global" {
		kind = "global_topic"
	}
	overlay := topicOverlays[topicSummaryKey(topic.Scope, topic.WorkspaceRoot, topic.TopicID)]
	node := ProjectNode{
		Key: kind + "_" + topic.TopicID, Kind: kind, Label: a.localizedTopicTitle(topic.Title, ""),
		Root: topic.WorkspaceRoot, TopicID: topic.TopicID, Turns: topic.Turns,
		TurnsState: string(topic.TurnsState), Health: string(topic.Health),
		CreatedAt: topic.CreatedAt, LastActivityAt: topic.LastActivityAt,
		Pinned: topic.Pinned, Open: overlay.open, Running: overlay.running, Status: overlay.status,
		Children: []ProjectNode{},
	}
	if len(topic.Sessions) <= 1 {
		return node
	}
	for _, session := range topic.Sessions {
		sessionKind := "session"
		if topic.Scope == "global" {
			sessionKind = "global_session"
		}
		sessionOverlay := sessionOverlays[sessionRuntimeKey(session.Path)]
		label := strings.TrimSpace(session.CustomTitle)
		if label == "" {
			label = strings.TrimSpace(session.Preview)
		}
		if label == "" {
			label = filepath.Base(session.Path)
		}
		node.Children = append(node.Children, ProjectNode{
			Key: projectSessionNodeKey(topic.Scope, session.Path), Kind: sessionKind,
			Label: label, Root: topic.WorkspaceRoot, TopicID: topic.TopicID,
			SessionPath: session.Path, Turns: session.Turns,
			TurnsState: string(session.TurnsState), Health: string(session.Health),
			CreatedAt: session.CreatedAt, LastActivityAt: session.LastActivityAt,
			Open: sessionOverlay.open, Running: sessionOverlay.running, Status: sessionOverlay.status,
			Recovered: session.Recovered, RecoveryReason: session.RecoveryReason,
			RecoveryDigest: session.RecoveryDigest, RecoveryParentID: session.ParentID,
			Children: []ProjectNode{},
		})
	}
	return node
}

func (a *App) ListProjectTopics(req ProjectTopicPageRequest) (ProjectTopicPage, error) {
	out := ProjectTopicPage{Items: []ProjectNode{}}
	catalog := a.sessionCatalog.Load()
	if catalog == nil {
		return a.metadataTopicPage(req), nil
	}
	page, err := catalog.ListTopics(a.bootContext(), sessioncatalog.TopicPageRequest{
		Scope: req.Scope, WorkspaceRoot: req.WorkspaceRoot, Cursor: req.Cursor,
		Limit: req.Limit, Query: req.Query, TimeFilter: req.TimeFilter,
	})
	if err != nil {
		return out, err
	}
	topicOverlays, sessionOverlays := a.catalogRuntimeOverlays()
	for _, topic := range page.Items {
		out.Items = append(out.Items, a.projectNodeFromCatalogTopic(topic, topicOverlays, sessionOverlays))
	}
	out.NextCursor = page.NextCursor
	out.Revision = page.Revision
	return out, nil
}

func (a *App) GetTopicSummary(key ProjectTopicKey) (ProjectNode, error) {
	if catalog := a.sessionCatalog.Load(); catalog != nil {
		topic, ok, err := catalog.GetTopic(a.bootContext(), sessioncatalog.TopicKey{
			Scope: key.Scope, WorkspaceRoot: key.WorkspaceRoot, TopicID: key.TopicID,
		})
		if err != nil {
			return ProjectNode{Children: []ProjectNode{}}, err
		}
		if ok {
			topicOverlays, sessionOverlays := a.catalogRuntimeOverlays()
			return a.projectNodeFromCatalogTopic(topic, topicOverlays, sessionOverlays), nil
		}
	}
	page, err := a.ListProjectTopics(ProjectTopicPageRequest{
		Scope: key.Scope, WorkspaceRoot: key.WorkspaceRoot, Limit: sessioncatalog.MaxLimit,
	})
	if err != nil {
		return ProjectNode{Children: []ProjectNode{}}, err
	}
	for _, node := range page.Items {
		if node.TopicID == key.TopicID {
			return node, nil
		}
	}
	return ProjectNode{Children: []ProjectNode{}}, nil
}

func (a *App) GetSessionCatalogStatus() SessionCatalogStatus {
	return a.currentSessionCatalogStatus()
}

func (a *App) RebuildSessionCatalog() error {
	if a == nil || a.shuttingDown.Load() {
		return errors.New("application is shutting down")
	}
	if !a.catalogRebuilding.CompareAndSwap(false, true) {
		return nil
	}
	go func() {
		a.stopSessionCatalog(250 * time.Millisecond)
		a.catalogRebuilding.Store(false)
		a.startSessionCatalog(true)
	}()
	return nil
}

// ListProjectTree is the one-release compatibility wrapper. It composes only
// catalog pages and project shells; it never migrates, scans, or decodes a
// session synchronously.
func (a *App) ListProjectTree() []ProjectNode {
	snapshot := a.GetProjectTreeSnapshot()
	hasGlobal := false
	for _, project := range snapshot.Projects {
		if project.Kind == "global_folder" {
			hasGlobal = true
			break
		}
	}
	if !hasGlobal && len(a.metadataProjectTopics("global", "")) > 0 {
		f := loadProjectsFile()
		label := strings.TrimSpace(f.GlobalTitle)
		if label == "" {
			label = "Global"
		}
		snapshot.Projects = append(snapshot.Projects, ProjectNode{
			Key: "global_folder", Kind: "global_folder", Label: label,
			Root: globalWorkspaceRoot(), ProjectColor: normalizeProjectColor(f.GlobalColor), Children: []ProjectNode{},
		})
		snapshot.Projects = applyPinnedProjectOrder(applyProjectTreeOrder(snapshot.Projects, f.SidebarOrder), f.PinnedProjects)
	}
	for index := range snapshot.Projects {
		project := &snapshot.Projects[index]
		scope := "project"
		root := project.Root
		if project.Kind == "global_folder" {
			scope = "global"
			root = ""
		}
		cursor := ""
		for {
			page, err := a.ListProjectTopics(ProjectTopicPageRequest{Scope: scope, WorkspaceRoot: root, Cursor: cursor, Limit: sessioncatalog.MaxLimit})
			if err != nil {
				break
			}
			project.Children = append(project.Children, page.Items...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(project.Children) == 0 {
			project.Children = a.metadataProjectTopics(scope, root)
		}
	}
	return snapshot.Projects
}

func (a *App) catalogSessionPathForTopic(scope, workspaceRoot, topicID string) string {
	if strings.TrimSpace(topicID) == "" {
		return ""
	}
	catalog := a.sessionCatalog.Load()
	if catalog == nil {
		return ""
	}
	topic, ok, err := catalog.GetTopic(a.bootContext(), sessioncatalog.TopicKey{Scope: scope, WorkspaceRoot: workspaceRoot, TopicID: topicID})
	if err != nil || !ok || len(topic.Sessions) == 0 {
		return ""
	}
	sort.SliceStable(topic.Sessions, func(i, j int) bool {
		return topic.Sessions[i].LastActivityAt > topic.Sessions[j].LastActivityAt
	})
	return topic.Sessions[0].Path
}
