# Reasonix Extension SDK for Go

Write [Reasonix](https://github.com/esengine/DeepSeek-Reasonix) extensions in
Go. An extension is a small sidecar process speaking **Extension Protocol
v1** (`reasonix.extension.v1`) over stdio: Reasonix launches it, hands it the
initialize handshake, and then drives intercepts, event observation,
extension-hosted provider streams, and structured UI surfaces.

The module is **standard library only** — zero dependencies.

## Install

```sh
go get github.com/esengine/DeepSeek-Reasonix/sdk/go
```

Requires Go 1.23+.

## Minimal example

```go
package main

import (
	"context"
	"encoding/json"
	"os"

	extension "github.com/esengine/DeepSeek-Reasonix/sdk/go"
)

type ext struct{}

func (ext) Initialize(_ context.Context, p extension.InitializeParams) (*extension.InitializeResult, error) {
	return &extension.InitializeResult{
		Name:          "my-ext",
		Version:       "0.1.0",
		Subscriptions: []string{"tool.before"},
	}, nil
}

func main() {
	err := extension.Serve(context.Background(), ext{}, extension.Options{
		Interceptors: map[string]extension.InterceptorFunc{
			"tool.before": func(_ context.Context, event string, payload json.RawMessage) (*extension.InterceptResult, error) {
				return extension.Continue(), nil // or Block / Replace / Allow / Deny
			},
		},
	})
	if err != nil {
		os.Exit(1)
	}
	// Serve returned nil: the host asked for shutdown. Exit 0.
}
```

Everything else is optional and declared through `Options`: an `Observer`
for fire-and-forget events, a `Provider` for extension-hosted model
providers, `UI` callbacks plus the `HostUI` client for structured surfaces
(status, cards, forms, notifications and blocking prompts — never HTML/JS),
`ReadContentRef`/`ResolveExternalized` for large externalized payloads, and a
`Shutdown` hook.

## Runnable example

[`examples/fullsidecar`](examples/fullsidecar/main.go) is the reference
extension: input rewriting (try the `/fs ` trigger), tool interception
(block + argument rewrite), system-prompt strategy replacement, a fake
streaming provider (text chunks, a tool call, usage), structured UI (status +
card on session start, a form prompt behind the `demo` action), and a clean
bounded shutdown — all in one small stdlib-only program.

```sh
go build -o /tmp/fullsidecar ./examples/fullsidecar
```

The binary speaks the protocol on stdin/stdout, so install it as a plugin
package runtime (or point the host-side conformance suite at it) rather than
running it interactively. It is driven end-to-end against the real host by
`internal/extension/conformance` in the Reasonix repository.

## Generated wire types

`types_generated.go` is produced from the host's frozen protocol registry by
`go run ./cmd/extension-protocol-gen -root .` (repository root). Edit nothing
in that file; the handwritten half of the type layer (validators, error
constructors, enum helpers) lives in `types_ext.go`.

## Protocol reference

- Method/DTO contract: [`docs/EXTENSION_PROTOCOL.generated.md`](../../docs/EXTENSION_PROTOCOL.generated.md)
- Canonical JSON schema: [`internal/extension/protocol/schema.generated.json`](../../internal/extension/protocol/schema.generated.json)
- Transport: strict JSON-RPC 2.0 over NDJSON (one object per line), integer
  request ids, object params, 8 MiB frames.

## Stability

Extension Protocol v1's compatibility promise applies: within major version
1, only optional fields, new enum values, and new methods are added; existing
required fields, method names, directions, limits, error reasons, and
semantics never change. This SDK tracks that contract — upgrading within v1
never breaks a compiled extension.
