package main

import (
	"context"

	"reasonix/internal/boot"
	"reasonix/internal/control"
)

// buildTabControllerBoot is a thin wrapper around boot.Build so the large
// controller assembly path can stay under function-size / complexity budgets.
func (a *App) buildTabControllerBoot(ctx context.Context, opts boot.Options) (control.SessionAPI, error) {
	return boot.Build(ctx, opts)
}
