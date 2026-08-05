/**
 * Resolve the workspace used to open a remote Serve page.
 *
 * A host's last-opened workspace wins over its configured default. Fresh
 * hosts fall back to the SSH login user's home directory, which the remote
 * bootstrap resolves from `~` before launching Serve.
 */
export function resolveRemoteWorkspace(lastWorkspace?: string, defaultWorkspace?: string): string {
  return lastWorkspace?.trim() || defaultWorkspace?.trim() || "~";
}
