export type FirebaseCrashSchemaState = {
  state: "absent" | "partial" | "complete";
  missing: string[];
};

export const firebaseCrashSchemaEntries: readonly string[];
export const firebaseCrashSchemaQuery: string;

export function classifyFirebaseCrashSchema(
  rows: Array<Record<string, unknown>>,
): FirebaseCrashSchemaState;
