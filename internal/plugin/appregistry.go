package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// AppInstance is one live MCP Apps surface: an unguessable token binding the
// Host, server, catalog generation, originating tool call, and the resource
// the App renders. Tokens are capability handles — possession alone authorizes
// nothing beyond reading that instance's identity; every App tool call still
// walks the full permission pipeline.
type AppInstance struct {
	Token       string
	Server      string
	Tool        string
	Generation  uint64
	CallID      string
	ResourceURI string
}

// appInstanceRegistry is the host's bounded set of live App instances. Max 32:
// beyond that the oldest instance is reclaimed, so a runaway App cannot pin
// memory. Server disconnect reclaims every instance of that server.
type appInstanceRegistry struct {
	mu        sync.Mutex
	instances map[string]*AppInstance
	order     []string
}

const maxAppInstances = 32

func newAppInstanceRegistry() *appInstanceRegistry {
	return &appInstanceRegistry{instances: map[string]*AppInstance{}}
}

func (r *appInstanceRegistry) newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-grade; an App token must be unguessable.
		panic("plugin: app instance token entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Register creates and stores a new instance, evicting the oldest when the
// registry is full.
func (r *appInstanceRegistry) Register(server, tool string, generation uint64, callID, resourceURI string) *AppInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst := &AppInstance{
		Token: r.newToken(), Server: server, Tool: tool,
		Generation: generation, CallID: callID, ResourceURI: resourceURI,
	}
	r.instances[inst.Token] = inst
	r.order = append(r.order, inst.Token)
	for len(r.order) > maxAppInstances {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.instances, oldest)
	}
	return inst
}

// Lookup resolves a token to its live instance.
func (r *appInstanceRegistry) Lookup(token string) (*AppInstance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[token]
	return inst, ok
}

// Release drops one instance (tab closed, component unmounted).
func (r *appInstanceRegistry) Release(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.instances[token]; !ok {
		return
	}
	delete(r.instances, token)
	for i, t := range r.order {
		if t == token {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// ReleaseServer drops every instance of one server (disconnect path).
func (r *appInstanceRegistry) ReleaseServer(server string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, inst := range r.instances {
		if inst.Server == server {
			delete(r.instances, token)
		}
	}
	filtered := r.order[:0]
	for _, t := range r.order {
		if _, ok := r.instances[t]; ok {
			filtered = append(filtered, t)
		}
	}
	r.order = filtered
}

// Len reports the live instance count.
func (r *appInstanceRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.instances)
}

// RegisterAppInstance creates a live App instance on the host.
func (h *Host) RegisterAppInstance(server, tool string, generation uint64, callID, resourceURI string) *AppInstance {
	return h.appInstances.Register(server, tool, generation, callID, resourceURI)
}

// LookupAppInstance resolves a token against the host registry.
func (h *Host) LookupAppInstance(token string) (*AppInstance, bool) {
	return h.appInstances.Lookup(token)
}

// ReleaseAppInstance drops one instance.
func (h *Host) ReleaseAppInstance(token string) {
	h.appInstances.Release(token)
}

// AppInstanceTool resolves the App-callable tool an instance may invoke:
// same server, visibility includes "app", catalog generation unchanged.
func (h *Host) AppInstanceTool(token, rawToolName string) (toolRef, bool) {
	inst, ok := h.LookupAppInstance(token)
	if !ok {
		return toolRef{}, false
	}
	h.mu.RLock()
	var client *Client
	for _, c := range h.clients {
		if c.name == inst.Server && !c.closed.Load() {
			client = c
			break
		}
	}
	h.mu.RUnlock()
	if client == nil {
		return toolRef{}, false
	}
	client.toolsMu.RLock()
	defer client.toolsMu.RUnlock()
	if client.toolCatalog.generation != inst.Generation || client.toolCatalogStale() {
		return toolRef{}, false
	}
	for _, t := range client.toolCatalog.appAdapters {
		rt, ok := t.(*remoteTool)
		if ok && rt.rawName == rawToolName && rt.appCallable {
			return toolRef{server: inst.Server, tool: rt}, true
		}
	}
	return toolRef{}, false
}

type toolRef struct {
	server string
	tool   *remoteTool
}

// UITool exposes the App-callable tool for CSP assembly.
func (r toolRef) UITool() *remoteTool { return r.tool }

// ReadResourceForApp reads one ui resource for the Apps channel, returning the
// text content and declared mime type.
func (h *Host) ReadResourceForApp(ctx context.Context, server, uri string) (string, string, error) {
	h.mu.RLock()
	var client *Client
	for _, c := range h.clients {
		if c.name == server && !c.closed.Load() {
			client = c
			break
		}
	}
	h.mu.RUnlock()
	if client == nil {
		return "", "", fmt.Errorf("server %q not connected", server)
	}
	return client.readResourceWithMime(ctx, uri)
}

// readResourceWithMime reads one resource and returns its text plus declared
// mime type (empty when the server did not declare one).
func (c *Client) readResourceWithMime(ctx context.Context, uri string) (string, string, error) {
	res, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", "", err
	}
	var wire struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(res, &wire); err != nil {
		return "", "", err
	}
	if len(wire.Contents) == 0 {
		return "", "", fmt.Errorf("resource %q returned no contents", uri)
	}
	first := wire.Contents[0]
	return first.Text, first.MimeType, nil
}
