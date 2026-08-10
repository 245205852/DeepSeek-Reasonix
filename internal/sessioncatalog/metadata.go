package sessioncatalog

import (
	"context"
	"strings"
)

// SyncMetadata projects the small desktop project/topic registries. It never
// removes session-derived topics: an older CLI or a concurrently running
// Reasonix process may have written authoritative sidecars not yet reflected in
// desktop-projects.json.
func (c *Catalog) SyncMetadata(ctx context.Context, projects []ProjectRecord, topics []TopicMetadata) error {
	if c == nil || c.db == nil {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	roots := map[string]struct{}{}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_projects`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_topics SET metadata_present=0`); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, project := range projects {
		project.Scope, project.WorkspaceRoot = normalizeScope(project.Scope, project.WorkspaceRoot)
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_projects(
            scope,workspace_root,title,color,pinned,sort_order,updated_at
        ) VALUES(?,?,?,?,?,?,?) ON CONFLICT(scope,workspace_root) DO UPDATE SET
            title=excluded.title,color=excluded.color,pinned=excluded.pinned,
            sort_order=excluded.sort_order,updated_at=excluded.updated_at`,
			project.Scope, project.WorkspaceRoot, project.Title, project.Color,
			project.Pinned, project.SortOrder, c.opts.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return err
		}
		roots[project.WorkspaceRoot] = struct{}{}
	}
	for _, topic := range topics {
		topic.Scope, topic.WorkspaceRoot = normalizeScope(topic.Scope, topic.WorkspaceRoot)
		if strings.TrimSpace(topic.TopicID) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_topics(
            scope,workspace_root,topic_id,title,title_source,pinned,sort_order,
            turns,turns_state,created_at,last_activity_at,recovery_state,health,metadata_present
        ) VALUES(?,?,?,?,?,?,?,0,'valid',?,0,'','ok',1)
        ON CONFLICT(scope,workspace_root,topic_id) DO UPDATE SET
            title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE catalog_topics.title END,
            title_source=excluded.title_source,pinned=excluded.pinned,
            sort_order=excluded.sort_order,metadata_present=1,
            created_at=CASE WHEN excluded.created_at>0 THEN excluded.created_at ELSE catalog_topics.created_at END`,
			topic.Scope, topic.WorkspaceRoot, topic.TopicID, topic.Title,
			topic.TitleSource, topic.Pinned, topic.SortOrder, topic.CreatedAt); err != nil {
			_ = tx.Rollback()
			return err
		}
		roots[topic.WorkspaceRoot] = struct{}{}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_topics
        WHERE metadata_present=0 AND NOT EXISTS (
            SELECT 1 FROM catalog_sessions s WHERE s.scope=catalog_topics.scope
            AND s.workspace_root=catalog_topics.workspace_root AND s.topic_id=catalog_topics.topic_id
        )`); err != nil {
		_ = tx.Rollback()
		return err
	}
	revision, err := bumpRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.publishRevision(revision, mapKeys(roots), "metadata")
	return nil
}
