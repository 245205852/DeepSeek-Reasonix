import type { Env } from "./env";

const TOKEN_URL = "https://oauth2.googleapis.com/token";
const TOKEN_SCOPE = [
  "https://www.googleapis.com/auth/userinfo.email",
  "https://www.googleapis.com/auth/firebase.database",
].join(" ");
const TOKEN_REFRESH_SKEW_MS = 5 * 60 * 1000;
const REQUEST_TIMEOUT_MS = 5_000;

type Fetcher = typeof fetch;

type CachedToken = {
  clientEmail: string;
  value: string;
  expiresAt: number;
};

let cachedToken: CachedToken | undefined;
let tokenRequest: Promise<CachedToken> | undefined;

export type FirebaseCrashSample = Record<string, unknown> & {
  eventId: string;
  receivedAt: string;
};

export type FirebaseCrashGroupMeta = {
  fingerprint: string;
  kind: string;
  count: number;
  firstSeen: string;
  lastSeen: string;
  firstVersion: string;
  lastVersion: string;
  status: string;
  title: string;
  source: string;
  label: string;
  errorType: string;
  topFrame: string;
  severity: string;
  lastOS: string;
  lastArch: string;
  lastBuildCommit: string;
  lastChannel: string;
  regressedAt: string;
};

export type FirebaseCrashGroup = {
  meta?: FirebaseCrashGroupMeta;
  samples?: {
    first?: FirebaseCrashSample;
    latest?: Record<string, FirebaseCrashSample> | FirebaseCrashSample[];
  };
};

function base64url(data: Uint8Array | string): string {
  const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary).replace(/=/g, "").replace(/\+/g, "-").replace(/\//g, "_");
}

function privateKeyBytes(pem: string): Uint8Array {
  const normalized = pem.replace(/\\n/g, "\n").trim();
  const encoded = normalized
    .replace("-----BEGIN PRIVATE KEY-----", "")
    .replace("-----END PRIVATE KEY-----", "")
    .replace(/\s/g, "");
  if (!encoded) throw new Error("firebase private key is empty");
  const binary = atob(encoded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

async function serviceAccountAssertion(env: Env, now: number): Promise<string> {
  const clientEmail = env.FIREBASE_CLIENT_EMAIL?.trim();
  const privateKey = env.FIREBASE_PRIVATE_KEY;
  if (!clientEmail || !privateKey) throw new Error("firebase service account is not configured");
  const issuedAt = Math.floor(now / 1000);
  const header = base64url(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const claims = base64url(JSON.stringify({
    iss: clientEmail,
    scope: TOKEN_SCOPE,
    aud: TOKEN_URL,
    iat: issuedAt,
    exp: issuedAt + 3600,
  }));
  const unsigned = `${header}.${claims}`;
  const key = await crypto.subtle.importKey(
    "pkcs8",
    privateKeyBytes(privateKey),
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    key,
    new TextEncoder().encode(unsigned),
  );
  return `${unsigned}.${base64url(new Uint8Array(signature))}`;
}

async function requestAccessToken(env: Env, fetcher: Fetcher, now: number): Promise<CachedToken> {
  const assertion = await serviceAccountAssertion(env, now);
  const body = new URLSearchParams({
    grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
    assertion,
  });
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetcher(TOKEN_URL, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body,
      signal: controller.signal,
    });
    if (!response.ok) throw new Error(`firebase oauth failed with ${response.status}`);
    const payload = await response.json() as { access_token?: unknown; expires_in?: unknown };
    if (typeof payload.access_token !== "string" || !payload.access_token) {
      throw new Error("firebase oauth response omitted access_token");
    }
    const expiresIn = typeof payload.expires_in === "number" ? payload.expires_in : 3600;
    return {
      clientEmail: env.FIREBASE_CLIENT_EMAIL!.trim(),
      value: payload.access_token,
      expiresAt: now + Math.max(60, expiresIn) * 1000,
    };
  } finally {
    clearTimeout(timeout);
  }
}

async function accessToken(env: Env, fetcher: Fetcher, now = Date.now()): Promise<string> {
  const clientEmail = env.FIREBASE_CLIENT_EMAIL?.trim() ?? "";
  if (
    cachedToken?.clientEmail === clientEmail &&
    cachedToken.expiresAt - TOKEN_REFRESH_SKEW_MS > now
  ) {
    return cachedToken.value;
  }
  if (!tokenRequest) {
    tokenRequest = requestAccessToken(env, fetcher, now).finally(() => {
      tokenRequest = undefined;
    });
  }
  cachedToken = await tokenRequest;
  return cachedToken.value;
}

function databaseURL(env: Env): string {
  const raw = env.FIREBASE_DATABASE_URL?.trim();
  if (!raw) throw new Error("firebase database URL is not configured");
  const url = new URL(raw);
  if (url.protocol !== "https:" || !(
    url.hostname.endsWith(".firebaseio.com") ||
    url.hostname.endsWith(".firebasedatabase.app")
  )) {
    throw new Error("firebase database URL is not an approved Realtime Database host");
  }
  url.pathname = url.pathname.replace(/\/$/, "");
  url.search = "";
  url.hash = "";
  return url.toString().replace(/\/$/, "");
}

async function firebaseFetch(
  env: Env,
  path: string,
  init: RequestInit,
  fetcher: Fetcher,
  retryAuth = true,
): Promise<Response> {
  const token = await accessToken(env, fetcher);
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetcher(`${databaseURL(env)}/${path}.json`, {
      ...init,
      signal: controller.signal,
      headers: {
        "content-type": "application/json",
        ...init.headers,
        authorization: `Bearer ${token}`,
      },
    });
    if (response.status === 401 && retryAuth) {
      cachedToken = undefined;
      return firebaseFetch(env, path, init, fetcher, false);
    }
    return response;
  } finally {
    clearTimeout(timeout);
  }
}

export function firebaseConfigured(env: Env): boolean {
  return Boolean(
    env.FIREBASE_DATABASE_URL?.trim() &&
    env.FIREBASE_CLIENT_EMAIL?.trim() &&
    env.FIREBASE_PRIVATE_KEY,
  );
}

export async function writeFirebaseCrashGroup(
  env: Env,
  meta: FirebaseCrashGroupMeta,
  sample: FirebaseCrashSample,
  latestSlot: number | null,
  firstSample: boolean,
  fetcher: Fetcher = fetch,
): Promise<void> {
  if (latestSlot !== null && (!Number.isInteger(latestSlot) || latestSlot < 0 || latestSlot > 4)) {
    throw new Error("firebase latest sample slot is invalid");
  }
  const body: Record<string, unknown> = {
    meta,
  };
  if (latestSlot !== null) body[`samples/latest/${latestSlot}`] = sample;
  if (firstSample) body["samples/first"] = sample;
  const response = await firebaseFetch(
    env,
    `groups/${encodeURIComponent(meta.fingerprint)}`,
    { method: "PATCH", body: JSON.stringify(body) },
    fetcher,
  );
  if (!response.ok) throw new Error(`firebase database write failed with ${response.status}`);
}

export async function readFirebaseCrashGroup(
  env: Env,
  fingerprint: string,
  fetcher: Fetcher = fetch,
): Promise<FirebaseCrashGroup | null> {
  const response = await firebaseFetch(
    env,
    `groups/${encodeURIComponent(fingerprint)}`,
    { method: "GET" },
    fetcher,
  );
  if (!response.ok) throw new Error(`firebase database read failed with ${response.status}`);
  const value = await response.json() as unknown;
  if (value === null) return null;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new Error("firebase database group response is invalid");
  }
  return value as FirebaseCrashGroup;
}

export async function writeFirebaseGroupMeta(
  env: Env,
  fingerprint: string,
  meta: FirebaseCrashGroupMeta,
  fetcher: Fetcher = fetch,
): Promise<void> {
  const response = await firebaseFetch(
    env,
    `groups/${encodeURIComponent(fingerprint)}/meta`,
    { method: "PUT", body: JSON.stringify(meta) },
    fetcher,
  );
  if (!response.ok) throw new Error(`firebase database metadata write failed with ${response.status}`);
}

export async function deleteFirebaseCrashGroup(
  env: Env,
  fingerprint: string,
  fetcher: Fetcher = fetch,
): Promise<void> {
  const response = await firebaseFetch(
    env,
    `groups/${encodeURIComponent(fingerprint)}`,
    { method: "DELETE" },
    fetcher,
  );
  if (!response.ok) throw new Error(`firebase database delete failed with ${response.status}`);
}

export function resetFirebaseAuthForTests(): void {
  cachedToken = undefined;
  tokenRequest = undefined;
}
