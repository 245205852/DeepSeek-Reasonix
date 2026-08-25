import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Env } from "./env";
import {
  deleteFirebaseCrashGroup,
  readFirebaseCrashGroup,
  resetFirebaseAuthForTests,
  writeFirebaseCrashGroup,
  writeFirebaseGroupMeta,
  type FirebaseCrashGroupMeta,
} from "./firebase_rtdb";

async function privateKeyPEM(): Promise<string> {
  const pair = await crypto.subtle.generateKey(
    { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
    true,
    ["sign", "verify"],
  ) as CryptoKeyPair;
  const exported = await crypto.subtle.exportKey("pkcs8", pair.privateKey) as ArrayBuffer;
  const bytes = new Uint8Array(exported);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  const encoded = btoa(binary).match(/.{1,64}/g)?.join("\n") ?? "";
  return `-----BEGIN PRIVATE KEY-----\n${encoded}\n-----END PRIVATE KEY-----`;
}

const meta: FirebaseCrashGroupMeta = {
  fingerprint: "a".repeat(64),
  kind: "crash",
  count: 1,
  firstSeen: "2026-08-25T00:00:00.000Z",
  lastSeen: "2026-08-25T00:00:00.000Z",
  firstVersion: "v1.0.0",
  lastVersion: "v1.0.0",
  status: "open",
  title: "boom",
  source: "go",
  label: "panic",
  errorType: "error",
  topFrame: "main.go:<n>",
  severity: "high",
  lastOS: "linux",
  lastArch: "amd64",
  lastBuildCommit: "",
  lastChannel: "stable",
  regressedAt: "",
};

async function firebaseEnv(): Promise<Env> {
  return {
    FIREBASE_DATABASE_URL: "https://reasonix-test.asia-southeast1.firebasedatabase.app",
    FIREBASE_CLIENT_EMAIL: "crash-writer@example.iam.gserviceaccount.com",
    FIREBASE_PRIVATE_KEY: await privateKeyPEM(),
  } as Env;
}

describe("Firebase Realtime Database delivery", () => {
  beforeEach(() => resetFirebaseAuthForTests());
  afterEach(() => vi.useRealTimers());

  it("coalesces OAuth requests and writes only fixed group/sample paths", async () => {
    const env = await firebaseEnv();
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    const fetcher: typeof fetch = async (input, init) => {
      const url = String(input);
      calls.push({ url, init });
      if (url === "https://oauth2.googleapis.com/token") {
        return Response.json({ access_token: "short-lived-token", expires_in: 3600 });
      }
      return Response.json({ ok: true });
    };
    const sample = { eventId: "b".repeat(32), receivedAt: meta.firstSeen, message: "sanitized" };
    await Promise.all([
      writeFirebaseCrashGroup(env, meta, sample, 0, true, fetcher),
      writeFirebaseCrashGroup(env, { ...meta, count: 2 }, sample, 1, false, fetcher),
    ]);
    expect(calls.filter((call) => call.url.includes("oauth2.googleapis.com"))).toHaveLength(1);
    const writes = calls.filter((call) => call.url.includes("firebasedatabase.app"));
    expect(writes).toHaveLength(2);
    expect(writes[0].init?.headers).toMatchObject({ authorization: "Bearer short-lived-token" });
    const firstBody = JSON.parse(String(writes[0].init?.body));
    expect(firstBody).toMatchObject({ meta, "samples/first": sample, "samples/latest/0": sample });
    expect(JSON.stringify(firstBody)).not.toContain("installId");
  });

  it("refreshes once after a 401 without exposing credential response bodies", async () => {
    const env = await firebaseEnv();
    let tokenRequests = 0;
    const fetcher: typeof fetch = async (input, init) => {
      const url = String(input);
      if (url.includes("oauth2.googleapis.com")) {
        tokenRequests++;
        return Response.json({ access_token: `token-${tokenRequests}`, expires_in: 3600 });
      }
      const authorization = (init?.headers as Record<string, string>).authorization;
      return authorization === "Bearer token-1" ? new Response("expired", { status: 401 }) : Response.json({ ok: true });
    };
    await writeFirebaseCrashGroup(
      env,
      meta,
      { eventId: "c".repeat(32), receivedAt: meta.firstSeen },
      0,
      true,
      fetcher,
    );
    expect(tokenRequests).toBe(2);

    resetFirebaseAuthForTests();
    const secretBody = "private-key-material-must-not-leak";
    const failing: typeof fetch = async () => new Response(secretBody, { status: 403 });
    await expect(writeFirebaseCrashGroup(
      env,
      meta,
      { eventId: "d".repeat(32), receivedAt: meta.firstSeen },
      0,
      true,
      failing,
    )).rejects.not.toThrow(secretBody);
  });

  it("bounds an unresponsive OAuth token request", async () => {
    vi.useFakeTimers();
    const env = await firebaseEnv();
    let signalRequest!: () => void;
    const requestStarted = new Promise<void>((resolve) => { signalRequest = resolve; });
    const fetcher: typeof fetch = async (_input, init) => {
      signalRequest();
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      });
    };
    const pending = writeFirebaseCrashGroup(
      env,
      meta,
      { eventId: "1".repeat(32), receivedAt: meta.firstSeen },
      0,
      true,
      fetcher,
    );
    const rejected = expect(pending).rejects.toThrow("aborted");
    await requestStarted;
    await vi.advanceTimersByTimeAsync(5_001);
    await rejected;
  });

  it("rejects non-Firebase database hosts before transmitting a sample", async () => {
    const env = { ...await firebaseEnv(), FIREBASE_DATABASE_URL: "https://example.com" } as Env;
    let databaseCalls = 0;
    const fetcher: typeof fetch = async (input) => {
      if (String(input).includes("oauth2.googleapis.com")) {
        return Response.json({ access_token: "token", expires_in: 3600 });
      }
      databaseCalls++;
      return Response.json({ ok: true });
    };
    await expect(writeFirebaseCrashGroup(
      env,
      meta,
      { eventId: "e".repeat(32), receivedAt: meta.firstSeen },
      0,
      true,
      fetcher,
    )).rejects.toThrow("approved Realtime Database host");
    expect(databaseCalls).toBe(0);
  });

  it("reads, replaces metadata, and deletes a group through authenticated REST calls", async () => {
    const env = await firebaseEnv();
    const calls: Array<{ url: string; method: string; body?: BodyInit | null }> = [];
    const sample = { eventId: "f".repeat(32), receivedAt: meta.firstSeen, message: "sample" };
    const fetcher: typeof fetch = async (input, init) => {
      const url = String(input);
      if (url.includes("oauth2.googleapis.com")) {
        return Response.json({ access_token: "dashboard-token", expires_in: 3600 });
      }
      calls.push({ url, method: init?.method ?? "GET", body: init?.body });
      if ((init?.method ?? "GET") === "GET") {
        return Response.json({ meta, samples: { first: sample, latest: { 0: sample } } });
      }
      return Response.json({ ok: true });
    };
    const group = await readFirebaseCrashGroup(env, meta.fingerprint, fetcher);
    expect(group?.samples?.first).toEqual(sample);
    await writeFirebaseGroupMeta(env, meta.fingerprint, { ...meta, severity: "critical" }, fetcher);
    await deleteFirebaseCrashGroup(env, meta.fingerprint, fetcher);
    expect(calls.map((call) => call.method)).toEqual(["GET", "PUT", "DELETE"]);
    expect(calls[1].url.endsWith(`/groups/${meta.fingerprint}/meta.json`)).toBe(true);
    expect(JSON.parse(String(calls[1].body))).toMatchObject({ severity: "critical" });
  });
});
