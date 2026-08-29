import type { WireEvent, WireMCPInteraction } from "./types";

let sequence = 0;
let activeID = "";

/** Browser-only fixture for exercising the structured MCP elicitation flow. */
function createMockMCPInteraction(id: string): WireMCPInteraction {
  return {
    id,
    server: "github",
    mode: "form",
    message: "MCP server requests additional information",
    requestedSchema: {
      type: "object",
      required: ["code", "environment"],
      properties: {
        code: { type: "string", title: "Device code", default: "123-456" },
        environment: {
          type: "string",
          title: "Environment",
          enum: ["Production", "Staging", "Development"],
          default: "Staging",
        },
        remember: { type: "boolean", title: "Remember this choice", default: false },
      },
    },
  };
}

export async function showMockMCPInteraction(
  wait: (milliseconds: number) => Promise<void>,
  isCancelled: () => boolean,
  emit: (event: WireEvent) => void,
): Promise<void> {
  const id = `mock-mcp-interaction-${++sequence}`;
  activeID = id;
  await wait(250);
  if (isCancelled() || activeID !== id) return;
  emit({ kind: "mcp_interaction", mcpInteraction: createMockMCPInteraction(id) });
}

export function consumeMockMCPInteraction(id: string): boolean {
  if (activeID !== id) return false;
  activeID = "";
  return true;
}
