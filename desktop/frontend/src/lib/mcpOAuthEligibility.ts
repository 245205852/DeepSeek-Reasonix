import type { ServerView } from "./types";

export function canUseNativeMCPOAuth(s: ServerView): boolean {
  const transport = (s.transport || "").trim().toLowerCase();
  if (s.authConfigured || !["http", "streamable-http", "streamable_http"].includes(transport)) return false;
  try {
    const parsed = new URL((s.url || s.authUrl || "").trim());
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}
