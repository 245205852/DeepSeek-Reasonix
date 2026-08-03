# Reasonix Extensions

Extensions let a plugin package change what Reasonix does at runtime —
rewrite input, intercept tool calls, replace the system prompt, contribute
streaming model providers, publish structured UI, and ship prompts and
themes — using a stable, versioned contract.

Two kinds of plugin capabilities exist:

- **Declarative** (any plugin package): skills, agents, commands, prompts,
  hooks, MCP servers, and themes. These are files and configuration; they
  run with the host's normal permissions.
- **Code runtime** (Manifest v1 `runtime` block): a sidecar process speaking
  the Extension Protocol. Code extensions are **full trust** — see the
  security section below before installing one.

## Installing and managing

Extensions install exactly like any plugin package:

```bash
reasonix plugin install git:github.com/owner/extension --dry-run   # preview
reasonix plugin install git:github.com/owner/extension --yes       # install
reasonix plugin show <name>                                        # details
reasonix plugin doctor <name>                                      # validate
```

For a plugin with a `runtime` block, the preview and `show` output include a
**FULL TRUST** block: the runtime command, the events it intercepts, the
replacement slots it owns, and its provider/UI capabilities. Installing,
updating, replacing, or `--link`ing is the authorization — there is no
second confirmation, and `--link` keeps trusting changed content. Only
install runtimes you trust completely.

## What extensions can do

- **Interceptors** — observe and rule on 17 hook points (input, tool calls,
  permission decisions, provider requests/responses, compaction, session
  lifecycle, frontend events). An interceptor can `continue`, `block` with a
  user-visible reason, or `replace` the payload; the host re-validates every
  replacement.
- **Replacement strategies** — single-owner slots (`system_prompt`,
  `context`, `provider_request`, `provider_response`, `compaction`,
  `session_policy`, `permission`, `frontend_events`, `tool:<name>`,
  `provider:<ref>`). One owner per slot across all installed plugins; a
  collision fails the runtime build with both sources named.
- **Streaming providers** — new models appear as
  `plugin/<plugin>/<provider>/<model>` in the model picker, streamed with
  the same text/reasoning/tool-call/usage semantics as built-in providers.
  The ref works everywhere a built-in ref does: `default_model`, `--model`,
  the CLI/Desktop/ACP pickers, and mid-session model switches — including on
  the very first boot.
- **Structured UI** — status entries, cards, forms, and notifications
  rendered natively in the CLI transcript, the Desktop app, and ACP clients
  (with text fallbacks), plus `/<plugin>:<action>` actions in the slash
  menu, the Desktop command palette, and ACP's discoverable commands.
- **Prompts and themes** — `/<plugin>:<name>` prompt templates and
  read-only plugin themes (`plugin:<plugin>:<theme>`) in Desktop Settings.

## Runtime reload

Changing an installed extension (install, update, enable/disable, or
`--link` content changes) never mutates a running turn. Reloading is one
fail-atomic operation with the same semantics everywhere — CLI `/reload`,
Desktop **Reload Runtime** (command palette), and the ACP vendor method
`_reasonix.io/session/reloadExtensions`:

1. If a turn or background work is running, exactly one reload is queued.
2. When idle, Reasonix starts new sidecars and builds a new runtime
   snapshot.
3. On full success it swaps atomically, carrying over the session path,
   transcript, approval grants, and goal/recovery state.
4. If the new build fails, the old runtime keeps working untouched.
5. Only after the swap are the old sidecars retired.

Each turn pins one runtime generation for the whole turn, tool batch, and
compaction — extension changes apply to the *next* turn, and a no-op reload
leaves the provider prompt-cache prefix byte-identical.

## Developing an extension

1. Add `apiVersion: "reasonix.io/plugin/v1"` to `reasonix-plugin.json` and
   declare `contributes` and (optionally) `runtime` — see
   `docs/PLUGIN_PACKAGES.md`.
2. Implement the sidecar. The Go SDK (`sdk/go`, standard library only)
   handles the transport, handshake, sequencing, content references, and
   shutdown for you; `docs/EXTENSION_PROTOCOL.md` describes the wire
   contract and `docs/EXTENSION_PROTOCOL.generated.md` is the generated
   method index.
3. Validate with `reasonix plugin doctor <name>` and iterate with
   `--link` + `/reload`.

## Compatibility

- Manifests without `apiVersion` parse exactly as before.
- Older Reasonix versions ignore extension-only state: the per-session
  `<session>.extensions.json` sidecar file, `plugin/...` model refs (they
  simply resolve as unavailable models), and the `extension_surface` /
  `extension_status` event kinds (older frontends drop unknown kinds; ACP
  clients without `reasonix.extensionSurface` get text fallbacks).
- `plugin-packages.json` keeps its existing schema; an enabled installed
  runtime *is* the trust record.

## Security model

A code extension runs outside the Reasonix sandbox with the unfiltered
inherited environment. It can read the full session and environment, bypass
permissions and workspace restrictions, and operate the machine directly;
its `permission.decision` "allow" overrides a host deny. In return the host
enforces:

- only plugins installed through the plugin flow can start a runtime —
  project configuration can never declare one;
- the handshake rejects any capability beyond the manifest;
- replacements are re-validated against each point's DTO and schema;
- sidecar output is credential-redacted before it reaches the UI, logs, or
  errors;
- a crashed sidecar fails its own operations explicitly — Reasonix never
  silently falls back to another model or strategy.
