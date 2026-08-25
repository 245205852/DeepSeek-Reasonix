import { describe, expect, it } from "vitest";
import {
  buildFirebaseGroups,
  firebaseOAuthGrantType,
} from "../scripts/migrate-firebase-data.mjs";

describe("Firebase retained-sample migration", () => {
  it("uses the OAuth JWT bearer grant expected by Google's token endpoint", () => {
    expect(firebaseOAuthGrantType).toBe("urn:ietf:params:oauth:grant-type:jwt-bearer");
  });

  it("keeps the first sample and maps the newest five into absolute ring slots", () => {
    const fingerprint = "a".repeat(64);
    const groups = buildFirebaseGroups([{
      fingerprint,
      kind: "crash",
      count: 8,
      first_seen: "2026-08-01T00:00:00Z",
      last_seen: "2026-08-08T00:00:00Z",
      first_version: "v1",
      last_version: "v2",
      status: "open",
      severity: "high",
    }], [1, 4, 5, 6, 7, 8].map((id) => ({
      id,
      fingerprint,
      kind: "crash",
      version: id === 1 ? "v1" : "v2",
      os: "linux",
      arch: "amd64",
      message: `sample-${id}`,
      created_at: `2026-08-0${id}T00:00:00Z`,
      device: "{}",
      breadcrumbs: "[]",
    })));
    const value = groups.get(fingerprint) as {
      meta: { count: number };
      samples: { first: { message: string }; latest: Record<string, { message: string; eventId: string }> };
    };
    expect(value.meta.count).toBe(8);
    expect(value.samples.first.message).toBe("sample-1");
    expect(Object.values(value.samples.latest).map((sample) => sample.message).sort()).toEqual([
      "sample-4", "sample-5", "sample-6", "sample-7", "sample-8",
    ]);
    expect(value.samples.latest[2].message).toBe("sample-8");
    expect(value.samples.latest[2].eventId).toMatch(/^[0-9a-f]{32}$/);
    expect(JSON.stringify(value)).not.toContain("installId");
  });
});
