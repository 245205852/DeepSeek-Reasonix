package sidecar

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"reasonix/internal/extension"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/pluginpkg"
)

// RequiredStartError reports a required runtime package that failed to start
// or hand shake. It fails the whole build; an optional package's failure is a
// warning instead. errors.As distinguishes the two at the boot call site.
type RequiredStartError struct {
	Plugin string
	Err    error
}

func (e *RequiredStartError) Error() string {
	return fmt.Sprintf("required extension runtime %q failed to start: %v", e.Plugin, e.Err)
}

func (e *RequiredStartError) Unwrap() error { return e.Err }

// LoadRuntimePackages returns the installed, ENABLED packages that declare a
// v1 runtime. This is the only enumeration the Manager ever launches from —
// see the package doc for the authorization invariant.
func LoadRuntimePackages(home string) ([]pluginpkg.InstalledPackage, []string) {
	installed, warnings := pluginpkg.LoadInstalled(home)
	var out []pluginpkg.InstalledPackage
	for _, item := range installed {
		if item.Package.Manifest.Runtime != nil {
			out = append(out, item)
		}
	}
	return out, warnings
}

// Manager owns every sidecar started for one runtime generation. It is the
// ONLY sidecar launch path, and its inputs are the pluginpkg installed state
// alone. It implements io.Closer so the kernel's RuntimeSet can retire it
// with its controller generation.
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client
	closed  bool
}

// StartPackages loads the installed enabled v1 runtime packages from home and
// spawns each one, running the initialize handshake against sessionCtx. A
// package whose manifest marks the runtime Required fails the whole call:
// everything started so far is shut down and the error is a
// *RequiredStartError. An optional package's failure is collected as a
// warning string and startup continues.
func StartPackages(ctx context.Context, home string, sessionCtx protocol.SessionContext, ui UIHandler) (*Manager, []string, error) {
	m := &Manager{clients: make(map[string]*Client)}
	packages, warnings := LoadRuntimePackages(home)
	for _, item := range packages {
		pluginID := item.Installed.Name
		client, err := StartClient(ctx, ClientOptions{
			Package:   item.Package,
			Installed: item.Installed,
			Session:   sessionCtx,
			UI:        ui,
			OnCrash: func(err error) {
				slog.Warn("extension sidecar crashed", "plugin", pluginID, "err", err)
			},
		})
		if err != nil {
			if item.Package.Manifest.Runtime.Required {
				_ = m.Close()
				return nil, warnings, &RequiredStartError{Plugin: pluginID, Err: err}
			}
			warnings = append(warnings, fmt.Sprintf("%s: optional extension runtime failed to start: %v", pluginID, err))
			continue
		}
		m.clients[pluginID] = client
	}
	return m, warnings, nil
}

// Client returns the client for one plugin ID, or nil.
func (m *Manager) Client(pluginID string) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clients[pluginID]
}

// Clients returns every live client ordered by plugin ID.
func (m *Manager) Clients() []*Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		out = append(out, client)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pluginID < out[j].pluginID })
	return out
}

// Close shuts every sidecar down in parallel; each client's own budgets
// bound the total. It is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			_ = c.Close()
		}(client)
	}
	wg.Wait()
	return nil
}

// Declaration-level kernel contribution payloads. They describe what a
// started sidecar declared; dispatch wiring arrives with stages 6-8.

// InterceptorDecl is the KindInterceptor payload for one manifest-declared
// interceptor point.
type InterceptorDecl struct {
	PluginID string
	Point    string
	Priority int
}

// StrategyDecl is the KindStrategy payload claiming one replacement slot. It
// implements the kernel's SlotClaimer so two runtimes claiming the same slot
// fail the build through ReplaceClaims.
type StrategyDecl struct {
	PluginID string
	Slots    []extension.Slot
}

// ReplacementSlots implements extension.SlotClaimer.
func (d StrategyDecl) ReplacementSlots() []extension.Slot {
	return append([]extension.Slot(nil), d.Slots...)
}

// ProviderDecl is the KindProvider payload for one handshake-declared
// extension-hosted provider.
type ProviderDecl struct {
	PluginID   string
	Descriptor protocol.ProviderDescriptor
}

// UIActionDecl is the KindUIAction payload for one handshake-declared action.
type UIActionDecl struct {
	PluginID string
	Decl     protocol.UIActionDecl
}

// Contributions renders every started client's declarations as kernel
// contributions: interceptor stubs per manifest intercept, strategy claims
// per manifest replaces, and provider / UI-action declarations from the
// validated handshake. Kernel-invalid IDs (whitespace, non-ref provider IDs)
// are skipped with a debug log rather than failing the whole snapshot.
func (m *Manager) Contributions() []extension.Contribution {
	var out []extension.Contribution
	for _, client := range m.Clients() {
		rt := client.rt
		source := extension.ContributionSource{
			Scope:    extension.ScopePlugin,
			PluginID: client.pluginID,
			Version:  client.version,
			Origin:   "extension-runtime",
		}
		for _, point := range rt.Intercepts {
			if !kernelID(point) {
				slog.Debug("sidecar: skipping interceptor point outside the kernel ID contract", "plugin", client.pluginID, "point", point)
				continue
			}
			out = append(out, extension.Contribution{
				Kind:     extension.KindInterceptor,
				ID:       point,
				Source:   source,
				Priority: rt.Priority,
				Payload:  InterceptorDecl{PluginID: client.pluginID, Point: point, Priority: rt.Priority},
			})
		}
		for _, slot := range rt.Replaces {
			if !kernelID(slot) {
				slog.Debug("sidecar: skipping replacement slot outside the kernel ID contract", "plugin", client.pluginID, "slot", slot)
				continue
			}
			out = append(out, extension.Contribution{
				Kind:     extension.KindStrategy,
				ID:       slot,
				Source:   source,
				Priority: rt.Priority,
				Payload:  StrategyDecl{PluginID: client.pluginID, Slots: []extension.Slot{extension.Slot(slot)}},
			})
		}
		result := client.Handshake()
		prefix := "plugin/" + client.pluginID + "/"
		for _, desc := range result.Providers {
			ref := strings.TrimPrefix(desc.Ref, prefix)
			if !extension.IsProviderRef(ref) {
				slog.Debug("sidecar: skipping provider ref outside the kernel ID contract", "plugin", client.pluginID, "ref", desc.Ref)
				continue
			}
			out = append(out, extension.Contribution{
				Kind:    extension.KindProvider,
				ID:      ref,
				Source:  source,
				Payload: ProviderDecl{PluginID: client.pluginID, Descriptor: desc},
			})
		}
		for _, decl := range result.UIActions {
			if !kernelID(decl.ActionID) {
				slog.Debug("sidecar: skipping UI action outside the kernel ID contract", "plugin", client.pluginID, "action", decl.ActionID)
				continue
			}
			out = append(out, extension.Contribution{
				Kind:    extension.KindUIAction,
				ID:      decl.ActionID,
				Source:  source,
				Payload: UIActionDecl{PluginID: client.pluginID, Decl: decl},
			})
		}
	}
	return out
}

// kernelID mirrors the kernel's generic ID hygiene (non-empty, no
// whitespace); boot's legacy assembly uses the same rule.
func kernelID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && !strings.ContainsAny(id, " \t\n")
}
