// Canonical parsing and validation for local file URLs used by Markdown links.

/**
 * Returns the decoded filesystem path represented by a local file URL.
 * The scheme check is intentionally case-sensitive so `FILE:` cannot bypass
 * the Markdown URL allowlist and reach a browser or native opener.
 */
export function localPathFromHref(href?: string): string | null {
  if (!href || !href.startsWith("file://")) return null;

  try {
    const url = new URL(href);
    if (url.protocol !== "file:") return null;
    if (url.username || url.password || url.port) return null;

    let path = decodeURIComponent(url.pathname);
    if (url.hostname) path = `//${url.hostname}${path}`;

    // file:///D:/... has a URL root slash that is not part of the Windows
    // drive path. Multiple leading slashes are the slash-form UNC variant.
    if (/^\/[A-Za-z]:\//.test(path)) path = path.slice(1);
    if (path.startsWith("//")) path = `//${path.replace(/^\/+/, "")}`;
    if (/^[A-Za-z]:\//.test(path)) return path;
    if (!path.startsWith("/")) return null;
    return path;
  } catch {
    return null;
  }
}

export function isLocalFileHref(href?: string): boolean {
  return localPathFromHref(href) !== null;
}
