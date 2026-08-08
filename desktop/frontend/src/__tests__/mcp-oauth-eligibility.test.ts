import { canUseNativeMCPOAuth } from "../lib/mcpOAuthEligibility";
import type { ServerView } from "../lib/types";

function server(overrides: Partial<ServerView>): ServerView {
  return {
    name: "server", transport: "http", status: "failed", configured: true,
    autoStart: true, tools: 0, prompts: 0, resources: 0,
    url: "https://mcp.example.test/mcp", ...overrides,
  };
}

if (!canUseNativeMCPOAuth(server({}))) throw new Error("eligible Streamable HTTP server should offer OAuth");
if (canUseNativeMCPOAuth(server({ transport: "stdio", url: undefined }))) throw new Error("stdio must keep its retry path");
if (canUseNativeMCPOAuth(server({ transport: "sse" }))) throw new Error("legacy SSE must keep its retry path");
if (canUseNativeMCPOAuth(server({ authConfigured: true }))) throw new Error("static authentication must take precedence");
