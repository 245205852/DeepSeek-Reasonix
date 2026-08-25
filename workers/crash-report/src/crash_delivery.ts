import type { Env } from "./env";
import { firebaseConfigured } from "./firebase_rtdb";

export const FIREBASE_OUTBOX_LIMIT = 5_000;
export const FIREBASE_OUTBOX_BATCH = 200;
const FIREBASE_GROUP_LEASE_MS = 60_000;

export type CrashStorageMode = "d1" | "dual" | "firebase";

export type FirebaseOutboxRow = {
  event_id: string;
  fingerprint: string;
  payload: string;
  state: "queued" | "processing" | "projected";
  attempts: number;
  next_attempt_at: string;
  created_at: string;
  updated_at: string;
};

export type FirebaseProjectionReceipt = {
  group_count: number;
  latest_slot: number;
  first_sample: number;
};

export function crashStorageMode(env: Env): CrashStorageMode {
  const raw = env.CRASH_STORAGE_MODE?.trim().toLowerCase() || "d1";
  if (raw === "d1" || raw === "dual" || raw === "firebase") return raw;
  throw new Error(`invalid crash storage mode ${raw}`);
}

export function firebaseStorageReady(env: Env): boolean {
  return crashStorageMode(env) === "d1" || firebaseConfigured(env);
}

export async function enqueueFirebaseCrash(
  env: Env,
  eventId: string,
  fingerprint: string,
  payload: string,
  now: string,
): Promise<"inserted" | "duplicate" | "full"> {
  const result = await env.DB.prepare(
    `INSERT OR IGNORE INTO firebase_crash_outbox (
       event_id, fingerprint, payload, state, attempts, next_attempt_at, created_at, updated_at
     )
     SELECT ?1, ?2, ?3, 'queued', 0, ?4, ?4, ?4
     WHERE (SELECT COUNT(*) FROM firebase_crash_outbox) < ?5
       AND NOT EXISTS (SELECT 1 FROM firebase_crash_receipts WHERE event_id = ?1)`,
  ).bind(eventId, fingerprint, payload, now, FIREBASE_OUTBOX_LIMIT).run();
  if (Number(result.meta?.changes ?? 0) > 0) return "inserted";
  const existing = await env.DB.prepare(
    `SELECT event_id FROM firebase_crash_outbox WHERE event_id = ?1
     UNION ALL SELECT event_id FROM firebase_crash_receipts WHERE event_id = ?1 LIMIT 1`,
  ).bind(eventId).first<{ event_id: string }>();
  return existing ? "duplicate" : "full";
}

export async function claimFirebaseCrash(env: Env, eventId: string, now: string): Promise<boolean> {
  const result = await env.DB.prepare(
    `UPDATE firebase_crash_outbox SET state = 'processing', updated_at = ?2
     WHERE event_id = ?1 AND (
       state = 'queued' OR (state = 'processing' AND datetime(updated_at) < datetime('now', '-10 minutes'))
     )`,
  ).bind(eventId, now).run();
  return Number(result.meta?.changes ?? 0) > 0;
}

export function projectionCompletionStatements(
  db: D1Database,
  eventId: string,
  fingerprint: string,
  now: string,
): D1PreparedStatement[] {
  return [
    db.prepare(
      `INSERT OR IGNORE INTO firebase_crash_receipts (
         event_id, projected_at, group_count, latest_slot, first_sample
       )
       SELECT ?1, ?2, count, (count - 1) % 5, CASE WHEN count = 1 THEN 1 ELSE 0 END
       FROM groups WHERE fingerprint = ?3`,
    ).bind(eventId, now, fingerprint),
    db.prepare(
      "UPDATE firebase_crash_outbox SET state = 'projected', updated_at = ?2 WHERE event_id = ?1",
    ).bind(eventId, now),
  ];
}

export async function firebaseProjectionReceipt(
  env: Env,
  eventId: string,
): Promise<FirebaseProjectionReceipt | null> {
  return env.DB.prepare(
    "SELECT group_count, latest_slot, first_sample FROM firebase_crash_receipts WHERE event_id = ?1",
  ).bind(eventId).first<FirebaseProjectionReceipt>();
}

export async function firebaseProjectionExists(env: Env, eventId: string): Promise<boolean> {
  return Boolean(await env.DB.prepare(
    "SELECT event_id FROM firebase_crash_receipts WHERE event_id = ?1",
  ).bind(eventId).first());
}

export async function firebaseOutboxExists(env: Env, eventId: string): Promise<boolean> {
  return Boolean(await env.DB.prepare(
    "SELECT event_id FROM firebase_crash_outbox WHERE event_id = ?1",
  ).bind(eventId).first());
}

export async function acquireFirebaseGroupLease(
  env: Env,
  fingerprint: string,
  now = new Date(),
): Promise<string | null> {
  const owner = crypto.randomUUID();
  const acquiredAt = now.toISOString();
  const expiresAt = new Date(now.getTime() + FIREBASE_GROUP_LEASE_MS).toISOString();
  await env.DB.prepare(
    `INSERT INTO firebase_crash_group_leases (fingerprint, owner, expires_at)
     VALUES (?1, ?2, ?3)
     ON CONFLICT (fingerprint) DO UPDATE SET owner = ?2, expires_at = ?3
     WHERE datetime(firebase_crash_group_leases.expires_at) <= datetime(?4)`,
  ).bind(fingerprint, owner, expiresAt, acquiredAt).run();
  const current = await env.DB.prepare(
    "SELECT owner FROM firebase_crash_group_leases WHERE fingerprint = ?1",
  ).bind(fingerprint).first<{ owner: string }>();
  return current?.owner === owner ? owner : null;
}

export async function releaseFirebaseGroupLease(
  env: Env,
  fingerprint: string,
  owner: string,
): Promise<void> {
  await env.DB.prepare(
    "DELETE FROM firebase_crash_group_leases WHERE fingerprint = ?1 AND owner = ?2",
  ).bind(fingerprint, owner).run();
}

export async function markFirebaseDelivered(env: Env, eventId: string): Promise<void> {
  await env.DB.prepare("DELETE FROM firebase_crash_outbox WHERE event_id = ?1").bind(eventId).run();
}

export async function recordFirebaseRetry(
  env: Env,
  eventId: string,
  state: FirebaseOutboxRow["state"],
  attempts: number,
): Promise<void> {
  const delaySeconds = Math.min(24 * 60 * 60, 30 * 2 ** Math.min(attempts, 11));
  const now = new Date();
  const next = new Date(now.getTime() + delaySeconds * 1000).toISOString();
  await env.DB.prepare(
    `UPDATE firebase_crash_outbox
     SET state = ?2, attempts = attempts + 1, next_attempt_at = ?3, updated_at = ?4
     WHERE event_id = ?1`,
  ).bind(eventId, state, next, now.toISOString()).run();
}

export async function dueFirebaseCrashes(env: Env): Promise<FirebaseOutboxRow[]> {
  const result = await env.DB.prepare(
    `SELECT event_id, fingerprint, payload, state, attempts, next_attempt_at, created_at, updated_at
     FROM firebase_crash_outbox
     WHERE next_attempt_at <= ?1 AND (
       state IN ('queued', 'projected') OR
       (state = 'processing' AND datetime(updated_at) < datetime('now', '-10 minutes'))
     )
     ORDER BY created_at LIMIT ?2`,
  ).bind(new Date().toISOString(), FIREBASE_OUTBOX_BATCH).all<FirebaseOutboxRow>();
  return result.results;
}

export async function purgeFirebaseDeliveryState(env: Env): Promise<void> {
  await env.DB.batch([
    env.DB.prepare("DELETE FROM firebase_crash_outbox WHERE datetime(created_at) < datetime('now', '-30 days')"),
    env.DB.prepare("DELETE FROM firebase_crash_receipts WHERE datetime(projected_at) < datetime('now', '-90 days')"),
    env.DB.prepare("DELETE FROM firebase_crash_group_leases WHERE datetime(expires_at) < datetime('now')"),
  ]);
}
