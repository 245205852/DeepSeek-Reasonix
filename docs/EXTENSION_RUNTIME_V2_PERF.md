# Extension Runtime v2 — Performance baselines

Soft CI thresholds live in `internal/extension/bench_threshold_test.go`.
Benchmarks live in `internal/extension/benchmark_test.go`.

## Targets (developer machine / CI soft fail)

| Operation | N | Soft upper bound |
| --- | --- | --- |
| `BuildDependencyGraph` | 32 components | < 50ms |
| `DiffRuntimePlan` no-op | same graph | < 20ms |
| `EffectScope.Dispose` | 64 effects | < 50ms |

## Incremental vs full rebuild

- **No-op / interceptor / UI / provider / MCP-only** (`RebuildFrom` + true subgraph patch): must **not** call `BuildRuntime`. Metrics: `NoOpRebuilds` / `SubgraphRebuilds`.
- **Full** (`SubgraphSidecar` / `SubgraphFull`): `FullRebuilds` + full `BuildRuntime`.

Measure:

```bash
go test ./internal/extension/ -run 'TestGraphAndPlanLatencyBaseline|TestEffectScopeDisposeBaseline' -count=1
go test ./internal/extension/ -bench 'BenchmarkDependencyGraphAndPlan|BenchmarkExtensionKernelStartup' -benchmem -count=3
go test ./internal/boot/ -run 'TestIntegrationNoOpDoesNotBuildNewController|TestRebuildFromNoOp' -count=1
```

## Cache hit expectation

No-op and UI/interceptor-only plans keep `RuntimeSnapshot.CacheHash` stable
(`shouldReuseSnapshot`). Provider/MCP-only may change cache when tools/providers
move; discovery of skills/commands/hooks is still skipped when `ReuseAssembly`
is retained.

## Sidecar start / drain

`StartPackagesWithPlan` adopts Unchanged clients (`SidecarAdopts` metric) and
only starts Added/Reloaded. Drain uses `Manager.DrainPlan` after publish.
Drain TTL defaults to 30s; force-expire fires registered cancel callbacks then
writes `drain-timeout-<gen>` receipts.
