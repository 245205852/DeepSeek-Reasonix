package sessioncatalog

import (
	"context"
	"database/sql"
	"errors"
)

func (c *Catalog) directoryScanCanSkip(ctx context.Context, path, signature string) (bool, error) {
	// Trust a ready marker only when its session and topic projections match
	// the completed directory scan in the same SQLite read snapshot.
	var expected, present, unprojected, missing int
	err := c.db.QueryRowContext(ctx, `SELECT total,
		(SELECT COUNT(*) FROM catalog_sessions WHERE directory=? AND missing_since=0),
		(SELECT COUNT(*) FROM catalog_sessions s
		 WHERE s.directory=? AND s.missing_since=0 AND s.topic_id<>''
		 AND NOT EXISTS (
			 SELECT 1 FROM catalog_topics t
			 WHERE t.scope=s.scope AND t.workspace_root=s.workspace_root AND t.topic_id=s.topic_id
		 )),
		(SELECT COUNT(*) FROM catalog_sessions WHERE directory=? AND missing_since>0)
		FROM catalog_directories
		WHERE path=? AND signature=? AND state='ready'`,
		path, path, path, path, signature).Scan(&expected, &present, &unprojected, &missing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return present == expected && unprojected == 0 && missing == 0, nil
}
