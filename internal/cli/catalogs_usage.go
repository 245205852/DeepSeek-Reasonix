package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"reasonix/internal/config"
	"reasonix/internal/usagecatalog"
)

func init() {
	registerCatalogCommand(catalogCommand{name: "usage", path: usagecatalog.DefaultPath, reindex: reindexUsageCatalog})
}

func reindexUsageCatalog(args []string) int {
	fs := flag.NewFlagSet("catalogs reindex usage", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print status as JSON")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	catalog, err := usagecatalog.Open(context.Background(), "")
	if err == nil {
		err = catalog.ReconcileDir(context.Background(), config.StatsDir())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer catalog.Close(context.Background())
	return printCatalogStatus(catalog.Status(), *jsonOut)
}
