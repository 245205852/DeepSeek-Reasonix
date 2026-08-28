import { useEffect, useRef, useState } from "react";
import { AppBridge, PostMessageTransport } from "@modelcontextprotocol/ext-apps/app-bridge";
import { app } from "../lib/bridge";
import type { MCPAppInstanceView } from "../lib/types";

// Height stays clamped to the Apps inline band.
const MIN_APP_HEIGHT = 120;
const MAX_APP_HEIGHT = 720;

function clampHeight(px: number): number {
  if (!Number.isFinite(px)) return MIN_APP_HEIGHT;
  return Math.min(MAX_APP_HEIGHT, Math.max(MIN_APP_HEIGHT, Math.round(px)));
}

function nonceFromOuterURL(outerUrl: string): string {
  try {
    return new URL(outerUrl).searchParams.get("nonce") ?? "";
  } catch {
    return "";
  }
}

// MCPAppCard mounts one live App instance behind the double-iframe sandbox:
// the outer iframe is the per-server loopback relay (fetched from
// MCPOpenAppInstance); the AppBridge drives the Host RPC surface from this
// side. Teardown sends ui/resource-teardown and force-closes after 1s.
export function MCPAppCard({
  instance,
  onDispose,
}: {
  instance: MCPAppInstanceView;
  onDispose?: (instanceToken: string) => void;
}) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const [height, setHeight] = useState(MIN_APP_HEIGHT);

  useEffect(() => {
    const frame = iframeRef.current;
    if (!frame) return;
    let closed = false;
    let teardownTimer: ReturnType<typeof setTimeout> | undefined;
    const bridge = new AppBridge(
      null,
      { name: "reasonix", version: "desktop" },
      { openLinks: {}, serverTools: {}, logging: {} },
    );

    bridge.oncalltool = async (params) => {
      const raw = await app.MCPAppCallTool(instance.instanceToken, params.name, params.arguments ?? {});
      return { content: [{ type: "text", text: raw }] };
    };
    bridge.onopenlink = async (params) => {
      await app.MCPOpenAppLink(params.url);
      return {};
    };

    const nonce = nonceFromOuterURL(instance.outerUrl);
    const onFrameLoad = () => {
      const target = frame.contentWindow;
      if (!target) return;
      target.postMessage({ __mcpInit: nonce }, "*");
      void bridge.connect(new PostMessageTransport(target, target));
    };
    frame.addEventListener("load", onFrameLoad);

    const onMessage = (e: MessageEvent) => {
      const data = e.data as { method?: string; params?: { height?: number } } | string | null;
      if (!data || typeof data === "string") return;
      if (data.method === "notifications/ui/size-changed" || data.method === "ui/size-changed") {
        if (typeof data.params?.height === "number") setHeight(clampHeight(data.params.height));
      }
    };
    window.addEventListener("message", onMessage);

    return () => {
      if (closed) return;
      closed = true;
      window.removeEventListener("message", onMessage);
      frame.removeEventListener("load", onFrameLoad);
      teardownTimer = setTimeout(() => {
        void bridge.close();
        app.MCPCloseAppInstance(instance.instanceToken).catch(() => undefined);
        onDispose?.(instance.instanceToken);
      }, 0);
      setTimeout(() => {
        if (teardownTimer) clearTimeout(teardownTimer);
        void bridge.close();
      }, 1000);
    };
  }, [instance.instanceToken, instance.outerUrl, onDispose]);

  const src = `${instance.outerUrl}&src=${encodeURIComponent(instance.resourceQuery)}`;
  return (
    <div className="mcp-app-card" data-server={instance.server}>
      <iframe
        ref={iframeRef}
        className="mcp-app-frame"
        src={src}
        style={{ height: `${height}px` }}
        title={`MCP App: ${instance.server}/${instance.tool}`}
      />
    </div>
  );
}
