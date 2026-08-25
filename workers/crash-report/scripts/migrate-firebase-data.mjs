#!/usr/bin/env node

import { createHash, createPrivateKey, createSign } from "node:crypto";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { parseWranglerRows } from "./apply-diagnostics-v2.mjs";

const tokenURL = "https://oauth2.googleapis.com/token";
const scope = "https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/firebase.database";
export const firebaseOAuthGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer";

function base64url(value) {
  return Buffer.from(value).toString("base64url");
}

function text(value) {
  return typeof value === "string" ? value : value == null ? "" : String(value);
}

function json(value, fallback) {
  if (typeof value !== "string" || value === "") return fallback;
  try { return JSON.parse(value); } catch { return fallback; }
}

function eventID(fingerprint, id) {
  return createHash("sha256").update(`firebase-migration\n${fingerprint}\n${id}`).digest("hex").slice(0, 32);
}

function sample(row) {
  return {
    eventId: eventID(text(row.fingerprint), text(row.id)),
    receivedAt: text(row.created_at),
    kind: text(row.kind),
    version: text(row.version),
    os: text(row.os),
    arch: text(row.arch),
    message: text(row.message),
    device: json(row.device, {}),
    source: text(row.source),
    label: text(row.label),
    errorType: text(row.error_type),
    errorMessage: text(row.error_message),
    topFrame: text(row.top_frame),
    buildCommit: text(row.build_commit),
    channel: text(row.channel),
    language: text(row.language),
    view: text(row.view),
    breadcrumbs: json(row.breadcrumbs, []),
    componentStack: text(row.component_stack),
    stack: text(row.stack),
    occurredAt: text(row.occurred_at),
    webview2: json(row.webview2, undefined),
    webRuntime: json(row.web_runtime, undefined),
  };
}

export function buildFirebaseGroups(groupRows, reportRows) {
  const reports = new Map();
  for (const row of reportRows) {
    const fingerprint = text(row.fingerprint);
    const values = reports.get(fingerprint) ?? [];
    values.push(row);
    reports.set(fingerprint, values);
  }
  const output = new Map();
  for (const row of groupRows) {
    const fingerprint = text(row.fingerprint);
    const count = Number(row.count) || 0;
    const retained = (reports.get(fingerprint) ?? []).sort((a, b) => Number(a.id) - Number(b.id));
    const first = retained[0];
    const latestRows = retained.slice(-5);
    const latest = {};
    latestRows.forEach((report, index) => {
      latest[(count - latestRows.length + index + 5) % 5] = sample(report);
    });
    output.set(fingerprint, {
      meta: {
        fingerprint,
        kind: text(row.kind),
        count,
        firstSeen: text(row.first_seen),
        lastSeen: text(row.last_seen),
        firstVersion: text(row.first_version),
        lastVersion: text(row.last_version),
        status: text(row.status),
        title: text(row.title),
        source: text(row.source),
        label: text(row.label),
        errorType: text(row.error_type),
        topFrame: text(row.top_frame),
        severity: text(row.severity),
        lastOS: text(row.last_os),
        lastArch: text(row.last_arch),
        lastBuildCommit: text(row.last_build_commit),
        lastChannel: text(row.last_channel),
        regressedAt: text(row.regressed_at),
      },
      samples: {
        ...(first ? { first: sample(first) } : {}),
        latest,
      },
    });
  }
  return output;
}

function runWrangler(projectDir, database, query) {
  const executable = process.platform === "win32" ? "wrangler.cmd" : "wrangler";
  const wrangler = path.join(projectDir, "node_modules", ".bin", executable);
  const result = spawnSync(wrangler, ["d1", "execute", database, "--remote", "--json", "--command", query], {
    cwd: projectDir,
    encoding: "utf8",
    env: process.env,
    stdio: ["ignore", "pipe", "inherit"],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`wrangler exited with status ${result.status}`);
  return parseWranglerRows(result.stdout ?? "");
}

async function accessToken(email, privateKey) {
  const now = Math.floor(Date.now() / 1000);
  const header = base64url(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const claims = base64url(JSON.stringify({ iss: email, scope, aud: tokenURL, iat: now, exp: now + 3600 }));
  const unsigned = `${header}.${claims}`;
  const signer = createSign("RSA-SHA256");
  signer.update(unsigned);
  signer.end();
  const assertion = `${unsigned}.${signer.sign(createPrivateKey(privateKey.replace(/\\n/g, "\n")), "base64url")}`;
  const response = await fetch(tokenURL, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: firebaseOAuthGrantType, assertion }),
  });
  if (!response.ok) throw new Error(`Firebase OAuth failed with ${response.status}`);
  const body = await response.json();
  if (typeof body.access_token !== "string") throw new Error("Firebase OAuth response omitted access_token");
  return body.access_token;
}

function databaseURL(raw) {
  const url = new URL(raw);
  if (url.protocol !== "https:" || !(url.hostname.endsWith(".firebaseio.com") || url.hostname.endsWith(".firebasedatabase.app"))) {
    throw new Error("FIREBASE_DATABASE_URL must be an approved Realtime Database host");
  }
  return url.toString().replace(/\/$/, "");
}

async function upload(baseURL, token, groups) {
  let completed = 0;
  for (const [fingerprint, value] of groups) {
    const response = await fetch(`${baseURL}/groups/${encodeURIComponent(fingerprint)}.json`, {
      method: "PUT",
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: JSON.stringify(value),
      signal: AbortSignal.timeout(10_000),
    });
    if (!response.ok) throw new Error(`Firebase migration write failed with ${response.status} after ${completed} groups`);
    completed++;
    if (completed % 100 === 0) console.log(`Migrated ${completed}/${groups.size} groups.`);
  }
  console.log(`Migrated and verified HTTP success for ${completed} Firebase groups.`);
}

async function main() {
  const projectDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  const database = process.env.DIAGNOSTICS_D1_DATABASE || "reasonix-crash";
  const groups = runWrangler(projectDir, database, "SELECT * FROM groups ORDER BY fingerprint");
  const reports = runWrangler(projectDir, database, "SELECT * FROM reports ORDER BY fingerprint, id");
  const payloads = buildFirebaseGroups(groups, reports);
  const bytes = [...payloads.values()].reduce((total, value) => total + Buffer.byteLength(JSON.stringify(value)), 0);
  console.log(`Prepared ${payloads.size} groups and ${reports.length} retained samples (${bytes} JSON bytes).`);
  if (!process.argv.includes("--apply")) {
    console.log("Dry run only. Pass --apply with Firebase service-account environment variables to upload.");
    return;
  }
  const email = process.env.FIREBASE_CLIENT_EMAIL;
  const privateKey = process.env.FIREBASE_PRIVATE_KEY;
  const rawURL = process.env.FIREBASE_DATABASE_URL;
  if (!email || !privateKey || !rawURL) throw new Error("Firebase service-account environment variables are required for --apply");
  await upload(databaseURL(rawURL), await accessToken(email, privateKey), payloads);
}

const invokedPath = process.argv[1] ? path.resolve(process.argv[1]) : "";
if (invokedPath === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  });
}
