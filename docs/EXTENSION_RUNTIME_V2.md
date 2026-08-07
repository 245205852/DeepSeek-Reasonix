# Extension Runtime v2 (Spatiotemporal Composability)

English overview of the Reasonix plugin/runtime v2 model. Chinese: [EXTENSION_RUNTIME_V2.zh-CN.md](./EXTENSION_RUNTIME_V2.zh-CN.md).

## Protocol and manifest

- Plugin manifest: exact `apiVersion: reasonix.io/plugin/v2` only (no `v2.0`/`v2.1` aliases; v1 and legacy native rejected).
- Compatibility boundary: no v1 dual-read or automatic migration. Extension
  manifests were not publicly released on v1, so v2 is the first supported
  runtime manifest.
- Extension wire protocol: `reasonix.extension.v2` (major 2).
- Handshake `provides` must be a subset of the manifest provides ceiling.

## Runtime model

```text
RuntimeSnapshot = immutable config + dependency view
RuntimeSet / EffectScope = live resources for one generation
RuntimePlan = old → new transition (subgraph classification)
RuntimeOwner = one session lineage's gate + receipts + stream/file evidence
Controller = published generation consumer (admission bound to RuntimeOwner)
```

Component states: `Inactive → Preparing → Active → Draining → Inactive` (or `Failed`).

## Rebuild

- Prefer `boot.RebuildFrom(previousBuildResult, opts)`.
- No-op / interceptor / UI / provider / MCP-only plans use **true subgraph patch** (no `BuildRuntime`); set `ReusedController` so callers must not `Close` the old pointer.
- Provider/MCP subgraph: live sidecar contributions refresh interceptor/provider/UI catalog via `WithLiveContributions`; **system prompt + tool schemas + CacheHash stay stable** (backend roll only). Tool schema renames still require full rebuild.
- Narrow path is **stage → ready → commit** (fail-atomic):
  - **Stage**: start/adopt sidecars, build next dispatcher/resolver; does **not** install stream routers or `BindGeneration` on the UI hub.
  - **Ready**: await sidecar readiness.
  - **Commit**: install stream routers, bind UI generation, replace controller bindings, then publish.
  - On any stage/ready/commit failure: `RollbackPlanStart` reattaches Unchanged clients; pre-stage stream routers keep consuming `stream/chunk` / `stream/end`.
- **UI during stage**: the previous UI hub generation stays bound until commit. Sidecars that emit `host/ui/publish` or `host/ui/request` with the staged (next) generation during handshake/ready are **dropped as stale**. Protocol policy: do not rely on UI visibility before the runtime generation is published.
- After successful migration: **publish** new generation, then **drain** old sidecars.
- Drain timeout cancels registered work (controller when replaced, **host provider streams** via `HostStreamRegistry`, extension provider streams, StableProxy/MCP in-flight) then writes `drain-timeout` receipts.
- Draining controllers reject new turns (`turnDroppedDraining`).

## Diagnostics

```bash
reasonix doctor runtime
reasonix doctor runtime --json
reasonix plugin doctor <name>
```

Reports component status, plan, effect receipts, recoverability, and lifecycle metrics.

## Effect receipts

Irreversible external work is recorded in the current `RuntimeOwner`'s receipt
store. Independent sessions never share publish/drain state or recovery
evidence. Recovery never claims successful rollback for irreversible effects;
use the owner-scoped `AssessRecoverability(generation)` /
`DecideResume(generation)` methods.

- Provider stream open records `provider-submit:<id>` (irreversible).
- Drain timeout force-expire records `drain-timeout:<gen>` and is scheduled after every publish (`ScheduleDrainWatch` / doctor sweep).

## EffectScope ownership

Live resources for one generation are tracked on `RuntimeSet` / `EffectScope`:

| Resource | Tracker |
| --- | --- |
| Sidecar manager | Cancelable effect in activator |
| UI hub binding | `TrackUIHub` |
| MCP plugin host | Inventory + `session-resources` dispose |
| LSP manager | `TrackWatcher` inventory |
| Session cleanup chain | `TrackControllerCleanup` |
| Provider submit | `RecordProviderSubmit` receipt |

## Acceptance mapping

| Spec acceptance | Status |
| --- | --- |
| Clear owner per resource and session lineage | Done (`RuntimeOwner` + EffectScope wiring for sidecar/MCP/UI/LSP/stream/file receipts) |
| Activation failure never leaks / never publishes | Done |
| Missing deps → Inactive diagnostics | Done (structured missing requirement + Unavailable) |
| Subgraph-only rebuild (no full BuildRuntime) | Done for None/Interceptor/UI/Provider/MCP |
| Publish/drain order + drain cancel | Done |
| Irreversible never rollback-success | Done (recovery `AssessRuntimeResume`) |
| Strict v2-only native runtime manifest | Done (no v1 dual-read / auto-migration) |
| Cache stability guards | Done |
| Doctor explains inactive + resume | Done (CLI + desktop RuntimeDoctor UI) |
| No external runtime dependency | Done |

## Phase 5 decisions

- **Compatibility**: v2 is the first supported extension runtime manifest;
  install/doctor/boot do not dual-read v1 and do not auto-migrate it.
- **Performance**: see [EXTENSION_RUNTIME_V2_PERF.md](./EXTENSION_RUNTIME_V2_PERF.md).
- **README / SDK**: v2 manifest examples under `sdk/go/examples/*`; protocol gen remains the source of truth for wire types.
