export interface GitHubLinkInfo {
  kind: "issue" | "pull" | "commit";
  owner: string;
  repo: string;
  value: string;
  compactLabel: string;
}

export type LinkIconKind = "github" | "external" | "mail";

export function classifyLinkIcon(href?: string): LinkIconKind | null {
  if (!href) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }

  if (url.protocol === "mailto:") return "mail";
  if (url.protocol !== "https:" && url.protocol !== "http:") return null;
  if (url.protocol === "https:" && ["github.com", "www.github.com"].includes(url.hostname.toLowerCase())) {
    return "github";
  }
  return "external";
}

export function parseGitHubLink(href?: string): GitHubLinkInfo | null {
  if (!href) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  if (url.protocol !== "https:" || !["github.com", "www.github.com"].includes(url.hostname.toLowerCase())) {
    return null;
  }

  const parts = url.pathname.split("/").filter(Boolean);
  if (parts.length !== 4) return null;
  const [owner, repo, resource, value] = parts;
  if (!owner || !repo || !value) return null;

  if (resource === "issues" && /^\d+$/.test(value)) {
    return { kind: "issue", owner, repo, value, compactLabel: `#${value}` };
  }
  if (resource === "pull" && /^\d+$/.test(value)) {
    return { kind: "pull", owner, repo, value, compactLabel: `PR #${value}` };
  }
  if (resource === "commit" && /^[0-9a-f]{7,40}$/i.test(value)) {
    return { kind: "commit", owner, repo, value, compactLabel: value.slice(0, 7) };
  }
  return null;
}
