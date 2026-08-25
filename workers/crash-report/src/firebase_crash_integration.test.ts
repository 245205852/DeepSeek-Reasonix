import { afterEach, describe, expect, it, vi } from "vitest";
// @ts-expect-error Node 22+ provides node:sqlite; Worker production code does not import it.
import { DatabaseSync } from "node:sqlite";
import worker, { drainFirebaseCrashOutbox } from "./index";
import type { Env } from "./env";
import freshSchemaSQL from "../schema.sql?raw";
import { resetFirebaseAuthForTests } from "./firebase_rtdb";
import {
  acquireFirebaseGroupLease,
  dueFirebaseCrashes,
  purgeFirebaseDeliveryState,
  releaseFirebaseGroupLease,
} from "./crash_delivery";

const firebaseOAuthTokenURL = "https://oauth2.googleapis.com/token";

type SQLiteD1Statement = D1PreparedStatement & {
  execute(): D1Result;
};

function sqliteD1(db: DatabaseSync): D1Database {
  return {
    prepare(sql: string) {
      let binds: unknown[] = [];
      const statement = {
        bind(...values: unknown[]) {
          binds = values;
          return statement;
        },
        async first<T>() {
          return (db.prepare(sql).get(...binds) ?? null) as T | null;
        },
        async all<T>() {
          return { success: true, results: db.prepare(sql).all(...binds) as T[], meta: {} };
        },
        async run() {
          return statement.execute();
        },
        execute() {
          const result = db.prepare(sql).run(...binds);
          return { success: true, results: [], meta: { changes: Number(result.changes) } } as unknown as D1Result;
        },
        raw() { return Promise.resolve([]); },
      } as unknown as SQLiteD1Statement;
      return statement;
    },
    async batch(statements: D1PreparedStatement[]) {
      db.exec("BEGIN IMMEDIATE");
      try {
        const results = statements.map((statement) => (statement as SQLiteD1Statement).execute());
        db.exec("COMMIT");
        return results;
      } catch (error) {
        db.exec("ROLLBACK");
        throw error;
      }
    },
  } as unknown as D1Database;
}

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

async function integrationEnv(db: DatabaseSync): Promise<Env> {
  return {
    DB: sqliteD1(db),
    RATE_LIMITER: { async limit() { return { success: true }; } },
    CRASH_STORAGE_MODE: "firebase",
    FIREBASE_DATABASE_URL: "https://reasonix-test.asia-southeast1.firebasedatabase.app",
    FIREBASE_CLIENT_EMAIL: "crash-writer@example.iam.gserviceaccount.com",
    FIREBASE_PRIVATE_KEY: await privateKeyPEM(),
  } as unknown as Env;
}

function reportRequest(eventId = "a".repeat(32)): Request {
  const body = JSON.stringify({
    eventId,
    dedupKey: "b".repeat(64),
    installId: "c".repeat(32),
    kind: "crash",
    version: "v1.25.0",
    os: "linux",
    arch: "amd64",
    message: "panic at /home/alice/project/main.go:12",
    source: "go",
    label: "panic",
    errorType: "runtime.error",
    topFrame: "main.go:12",
  });
  return new Request("https://crash.reasonix.io/v1/report", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "content-length": String(new TextEncoder().encode(body).byteLength),
      "cf-connecting-ip": "127.0.0.1",
    },
    body,
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  resetFirebaseAuthForTests();
});

describe("Firebase-primary crash ingest", () => {
  it("serializes each group across isolates and protects a replacement lease from stale release", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    const fingerprint = "9".repeat(64);
    try {
      const first = await acquireFirebaseGroupLease(env, fingerprint, new Date("2026-08-25T00:00:00Z"));
      expect(first).toBeTruthy();
      expect(await acquireFirebaseGroupLease(env, fingerprint, new Date("2026-08-25T00:00:30Z"))).toBeNull();
      const replacement = await acquireFirebaseGroupLease(env, fingerprint, new Date("2026-08-25T00:01:01Z"));
      expect(replacement).toBeTruthy();
      await releaseFirebaseGroupLease(env, fingerprint, first!);
      expect(db.prepare("SELECT owner FROM firebase_crash_group_leases").get()).toEqual({ owner: replacement });
      await releaseFirebaseGroupLease(env, fingerprint, replacement!);
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_group_leases").get()).toEqual({ count: 0 });
    } finally {
      db.close();
    }
  });

  it("reclaims ISO-timestamped processing rows and purges expired delivery state", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    try {
      db.prepare(`INSERT INTO firebase_crash_outbox (
        event_id, fingerprint, payload, state, attempts, next_attempt_at, created_at, updated_at
      ) VALUES (?, ?, '{}', 'processing', 0, ?, ?, ?)`)
        .run("8".repeat(32), "7".repeat(64), "2000-01-01T00:00:00.000Z", "2000-01-01T00:00:00.000Z", "2000-01-01T00:00:00.000Z");
      db.prepare(`INSERT INTO firebase_crash_receipts (
        event_id, projected_at, group_count, latest_slot, first_sample
      ) VALUES (?, ?, 1, 0, 1)`).run("6".repeat(32), "2000-01-01T00:00:00.000Z");
      db.prepare("INSERT INTO firebase_crash_group_leases (fingerprint, owner, expires_at) VALUES (?, ?, ?)")
        .run("5".repeat(64), "expired", "2000-01-01T00:00:00.000Z");
      expect(await dueFirebaseCrashes(env)).toHaveLength(1);
      await purgeFirebaseDeliveryState(env);
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 0 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_receipts").get()).toEqual({ count: 0 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_group_leases").get()).toEqual({ count: 0 });
    } finally {
      db.close();
    }
  });

  it("stores no long-lived D1 sample and deduplicates a repeated eventId", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    const writes: Array<{ url: string; body: string }> = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === firebaseOAuthTokenURL) {
        return Response.json({ access_token: "token", expires_in: 3600 });
      }
      writes.push({ url, body: String(init?.body) });
      return Response.json({ ok: true });
    });
    try {
      expect((await worker.fetch(reportRequest(), env)).status).toBe(202);
      expect((await worker.fetch(reportRequest(), env)).status).toBe(202);
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 1 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM reports").get()).toEqual({ count: 0 });
      expect(db.prepare("SELECT events FROM report_daily").get()).toEqual({ events: 1 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 0 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_receipts").get()).toEqual({ count: 1 });
      expect(db.prepare("SELECT group_count, latest_slot, first_sample FROM firebase_crash_receipts").get())
        .toEqual({ group_count: 1, latest_slot: 0, first_sample: 1 });
      expect(writes).toHaveLength(1);
      const firebaseBody = writes[0].body;
      expect(firebaseBody).toContain("/home/_/project/main.go:12");
      expect(firebaseBody).not.toContain("\"installId\"");
      expect(firebaseBody).not.toContain("alice");
    } finally {
      db.close();
    }
  });

  it("buffers an unavailable Firebase write and retries without double-counting D1", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    let databaseAvailable = false;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === firebaseOAuthTokenURL) {
        return Response.json({ access_token: "token", expires_in: 3600 });
      }
      return databaseAvailable ? Response.json({ ok: true }) : new Response("unavailable", { status: 503 });
    });
    try {
      expect((await worker.fetch(reportRequest("d".repeat(32)), env)).status).toBe(202);
      expect(db.prepare("SELECT state FROM firebase_crash_outbox").get()).toEqual({ state: "projected" });
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 1 });
      db.prepare("UPDATE firebase_crash_outbox SET next_attempt_at = '2000-01-01T00:00:00.000Z'").run();
      databaseAvailable = true;
      await drainFirebaseCrashOutbox(env);
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 0 });
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 1 });
    } finally {
      db.close();
    }
  });

  it("queues a concurrent same-group report until the current projection and Firebase write release the lease", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    const bodies: Array<Record<string, unknown>> = [];
    let releaseFirstWrite!: () => void;
    const firstWriteReleased = new Promise<void>((resolve) => { releaseFirstWrite = resolve; });
    let signalFirstWrite!: () => void;
    const firstWriteStarted = new Promise<void>((resolve) => { signalFirstWrite = resolve; });
    let databaseWrites = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === firebaseOAuthTokenURL) {
        return Response.json({ access_token: "token", expires_in: 3600 });
      }
      databaseWrites++;
      bodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
      if (databaseWrites === 1) {
        signalFirstWrite();
        await firstWriteReleased;
      }
      return Response.json({ ok: true });
    });
    try {
      const first = worker.fetch(reportRequest("1".repeat(32)), env);
      await firstWriteStarted;
      expect((await worker.fetch(reportRequest("2".repeat(32)), env)).status).toBe(202);
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 1 });
      expect(db.prepare("SELECT state FROM firebase_crash_outbox WHERE event_id = ?").get("2".repeat(32)))
        .toEqual({ state: "queued" });
      releaseFirstWrite();
      expect((await first).status).toBe(202);
      await drainFirebaseCrashOutbox(env);
      expect(bodies.map((body) => (body.meta as { count: number }).count)).toEqual([1, 2]);
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 2 });
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 0 });
    } finally {
      releaseFirstWrite();
      db.close();
    }
  });

  it("returns 503 instead of growing beyond the 5000-row outbox cap", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    db.exec(`WITH RECURSIVE n(value) AS (
      SELECT 1 UNION ALL SELECT value + 1 FROM n WHERE value < 5000
    ) INSERT INTO firebase_crash_outbox (
      event_id, fingerprint, payload, state, attempts, next_attempt_at, created_at, updated_at
    ) SELECT printf('%032x', value), '${"e".repeat(64)}', '{}', 'queued', 0,
      '2026-08-25T00:00:00.000Z', '2026-08-25T00:00:00.000Z', '2026-08-25T00:00:00.000Z' FROM n`);
    const env = await integrationEnv(db);
    let networkCalls = 0;
    vi.stubGlobal("fetch", async () => {
      networkCalls++;
      return Response.json({ ok: true });
    });
    try {
      expect((await worker.fetch(reportRequest("f".repeat(32)), env)).status).toBe(503);
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 5000 });
      expect(networkCalls).toBe(0);
    } finally {
      db.close();
    }
  });

  it("does not let a stale retry overwrite a newer ring-buffer slot", async () => {
    const db = new DatabaseSync(":memory:");
    db.exec(freshSchemaSQL);
    const env = await integrationEnv(db);
    let databaseWrites = 0;
    const successfulBodies: string[] = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === firebaseOAuthTokenURL) {
        return Response.json({ access_token: "token", expires_in: 3600 });
      }
      databaseWrites++;
      if (databaseWrites === 1) return new Response("unavailable", { status: 503 });
      successfulBodies.push(String(init?.body));
      return Response.json({ ok: true });
    });
    try {
      for (const value of "1234567") {
        expect((await worker.fetch(reportRequest(value.repeat(32)), env)).status).toBe(202);
      }
      expect(db.prepare("SELECT count FROM groups").get()).toEqual({ count: 7 });
      db.prepare("UPDATE firebase_crash_outbox SET next_attempt_at = '2000-01-01T00:00:00.000Z'").run();
      await drainFirebaseCrashOutbox(env);
      const retry = JSON.parse(successfulBodies.at(-1) ?? "{}") as Record<string, unknown>;
      expect(retry.meta).toMatchObject({ count: 7 });
      expect(retry["samples/first"]).toMatchObject({ eventId: "1".repeat(32) });
      expect(Object.keys(retry).some((key) => key.startsWith("samples/latest/"))).toBe(false);
      expect(db.prepare("SELECT COUNT(*) AS count FROM firebase_crash_outbox").get()).toEqual({ count: 0 });
    } finally {
      db.close();
    }
  });
});
