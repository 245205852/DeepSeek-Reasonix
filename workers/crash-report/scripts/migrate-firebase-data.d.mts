export const firebaseOAuthGrantType: string;

export function buildFirebaseGroups(
  groupRows: Array<Record<string, unknown>>,
  reportRows: Array<Record<string, unknown>>,
): Map<string, Record<string, unknown>>;
