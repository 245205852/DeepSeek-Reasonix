export const firebaseOAuthGrantType: string;

export function classifyMigrationGroup(
  row: Record<string, unknown>,
  now?: Date,
): "active" | "compacted" | "archived";

export function canonicalJSONString(value: unknown): string;
export function contentDigest(value: unknown): string;

export function buildFirebaseGroups(
  groupRows: Array<Record<string, unknown>>,
  reportRows: Array<Record<string, unknown>>,
  now?: Date,
): Map<string, {
  state: "active" | "compacted" | "archived";
  value: Record<string, unknown> | null;
  firstEventId: string;
  reservedBytes: number;
}>;
