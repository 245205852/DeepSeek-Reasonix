// Provider model cache with single-flight deduplication and exponential
// backoff. Wraps the Wails-bound FetchProviderModels to avoid redundant
// network requests when the Settings > Model panel mounts or re-renders.
//
// Cache keyed by apiKeyEnv | baseUrl | modelsURL, TTL 60 s.

type CacheEntry = { at: number; models: string[] };

const cache = new Map<string, CacheEntry>();
const inflight = new Map<string, Promise<string[]>>();
const backoff = new Map<string, number>();

const TTL = 60_000;
const BACKOFF_INITIAL = 1000;
const BACKOFF_CAP = 60_000;

function cacheKey(p: { apiKeyEnv?: string; baseUrl?: string; modelsURL?: string }): string {
  return [
    (p.apiKeyEnv ?? "").trim(),
    (p.baseUrl ?? "").trim(),
    (p.modelsURL ?? "").trim(),
  ].join("|");
}

function currentBackoff(key: string): number {
  const d = backoff.get(key) ?? BACKOFF_INITIAL;
  return d;
}

function increaseBackoff(key: string): void {
  backoff.set(key, Math.min(currentBackoff(key) * 2, BACKOFF_CAP));
}

function resetBackoff(key: string): void {
  backoff.delete(key);
}

export async function cachedFetchProviderModels(
  fetchFn: (provider: any) => Promise<string[]>,
  provider: any,
  force = false,
): Promise<string[]> {
  const key = cacheKey(provider);

  // Cache hit (skip if forced refresh)
  if (!force) {
    const hit = cache.get(key);
    if (hit && Date.now() - hit.at < TTL) return hit.models;
  }

  // Single-flight: reuse in-flight promise
  const pending = inflight.get(key);
  if (pending) return pending;

  // Check backoff — if we're in a cool-down window, resolve empty
  const delay = currentBackoff(key);
  if (backoff.has(key) && delay >= BACKOFF_INITIAL * 2) {
    // At least one prior failure; skip eager retry and return empty
    return [];
  }

  const p = fetchFn(provider)
    .then((models) => {
      cache.set(key, { at: Date.now(), models });
      resetBackoff(key);
      return models;
    })
    .catch((err) => {
      increaseBackoff(key);
      throw err;
    })
    .finally(() => {
      inflight.delete(key);
    });

  inflight.set(key, p);
  return p;
}

/** Tell the caller whether this provider is in backoff. */
export function isBackingOff(provider: any): boolean {
  return backoff.has(cacheKey(provider));
}

/** Clear cache for a single provider (e.g. after key change). */
export function invalidateProviderCache(provider: any): void {
  const key = cacheKey(provider);
  cache.delete(key);
  resetBackoff(key);
}

/** Clear the entire model cache. */
export function clearModelCache(): void {
  cache.clear();
  backoff.clear();
  // inflight promises are left to resolve naturally
}

/**
 * Return true when the network connection is too slow for background
 * model-list refreshes.
 */
export function shouldSkipAutoRefresh(): boolean {
  const nav = navigator as any;
  const conn = nav.connection;
  if (!conn) return false;
  if (conn.saveData) return true;
  if (conn.effectiveType === "slow-2g" || conn.effectiveType === "2g") return true;
  return false;
}
