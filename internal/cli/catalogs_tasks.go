package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/taskcatalog"
)

func init() {
	registerCatalogCommand(catalogCommand{name: "tasks", path: taskcatalog.DefaultPath, reindex: reindexTaskCatalog})
}

func reindexTaskCatalog(args []string) int {
	fs := flag.NewFlagSet("catalogs reindex tasks", flag.ContinueOnError)
	var projects stringListFlag
	jsonOut := fs.Bool("json", false, "print status as JSON")
	fs.Var(&projects, "project", "project root to index; repeat for multiple projects")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	if len(projects) == 0 {
		for _, target := range defaultSessionCatalogTargets() {
			if strings.TrimSpace(target.WorkspaceRoot) != "" {
				projects = append(projects, target.WorkspaceRoot)
			}
		}
	}
	catalog, err := taskcatalog.Open(context.Background(), "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer catalog.Close(context.Background())
	for _, root := range projects {
		project, err := catalog.RegisterProject(context.Background(), root, filepath.Base(root))
		if err == nil {
			err = catalog.ReconcileProject(context.Background(), project)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	return printCatalogStatus(catalog.Status(), *jsonOut)
}
