package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"reasonix/internal/plugin"
)

// activeMCPRuntimeHost returns the active tab's MCP host.
func (a *App) activeMCPRuntimeHost() *plugin.Host {
	_, ctrl, _ := a.activeMCPRuntime()
	if ctrl == nil {
		return nil
	}
	return ctrl.Host()
}

// MCPAppInstanceView describes one live App surface for the frontend.
type MCPAppInstanceView struct {
	InstanceToken string `json:"instanceToken"`
	Server        string `json:"server"`
	Tool          string `json:"tool"`
	OuterURL      string `json:"outerUrl"`
	ResourceQuery string `json:"resourceQuery"`
}

// MCPOpenAppInstance registers a host App instance for a tool result and
// returns the double-iframe sandbox coordinates. The resource itself loads
// only after the frontend posts the init nonce.
func (a *App) MCPOpenAppInstance(server, tool string, generation uint64, callID, resourceURI string) (*MCPAppInstanceView, error) {
	host := a.activeMCPRuntimeHost()
	if host == nil {
		return nil, fmt.Errorf("no active MCP runtime")
	}
	inst := host.RegisterAppInstance(server, tool, generation, callID, resourceURI)
	outer, err := a.appOriginURL(server)
	if err != nil {
		host.ReleaseAppInstance(inst.Token)
		return nil, err
	}
	return &MCPAppInstanceView{
		InstanceToken: inst.Token,
		Server:        server,
		Tool:          tool,
		OuterURL:      outer,
		ResourceQuery: "/resource?token=" + inst.Token,
	}, nil
}

// MCPAppResourceDigest returns the current SHA-256 of a live instance's
// resource so history can detect a changed upstream before restoring UI.
func (a *App) MCPAppResourceDigest(instanceToken string) (string, error) {
	host := a.activeMCPRuntimeHost()
	if host == nil {
		return "", fmt.Errorf("no active MCP runtime")
	}
	inst, ok := host.LookupAppInstance(instanceToken)
	if !ok {
		return "", fmt.Errorf("unknown app instance")
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 30*1000*1000*1000)
	defer cancel()
	content, _, err := host.ReadResourceForApp(ctx, inst.Server, inst.ResourceURI)
	if err != nil {
		return "", err
	}
	return resourceDigest(content), nil
}

// MCPCloseAppInstance reclaims an instance (tab closed, component unmounted).
func (a *App) MCPCloseAppInstance(instanceToken string) {
	if host := a.activeMCPRuntimeHost(); host != nil {
		host.ReleaseAppInstance(instanceToken)
	}
}

// appLink grants are remembered per instance + origin for this process only.
type appLinkGrant struct {
	mu      chan struct{}
	granted map[string]bool
}

var appLinkGrants = struct {
	mu  chan struct{}
	per map[string]*appLinkGrant
}{mu: make(chan struct{}, 1), per: map[string]*appLinkGrant{}}

// MCPOpenAppLink opens a ui/open-link target after per-origin confirmation.
// The frontend asks first and shows the confirmation; this only routes to the
// system browser.
func (a *App) MCPOpenAppLink(rawURL string) error {
	runtime.BrowserOpenURL(a.ctx, rawURL)
	return nil
}

// MCPAppCallTool routes an App-initiated tools/call through the controller's
// gated channel: same server as the instance, visibility includes "app",
// catalog generation unchanged, and the ordinary permission policy decides.
func (a *App) MCPAppCallTool(instanceToken, toolName string, args json.RawMessage) (string, error) {
	_, ctrl, _ := a.activeMCPRuntime()
	if ctrl == nil {
		return "", fmt.Errorf("no active runtime")
	}
	caller, ok := ctrl.(interface {
		MCPAppCallTool(instanceToken, toolName string, args json.RawMessage) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("runtime does not support app tool calls")
	}
	return caller.MCPAppCallTool(instanceToken, toolName, args)
}
