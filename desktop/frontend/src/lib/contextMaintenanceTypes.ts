export type ContextMaintenanceStatus = "planned" | "applied" | "noop" | "blocked" | "failed";
export type ContextMaintenanceAction = "snip" | "prune" | "summary" | "native_tool_clear" | "noop";

export interface WireContextMaintenance {
  status?: ContextMaintenanceStatus;
  action?: ContextMaintenanceAction;
  trigger?: string;
  operationId?: string;
  inputTokens?: number;
  resultTokens?: number;
  savedTokens?: number;
  affectedToolResults?: number;
  projectionVersion?: number;
  cacheBreak?: boolean;
  reason?: string;
}

export interface ContextMaintenanceReceipt extends WireContextMaintenance {
  sourceProjection?: number;
  coveredCount?: number;
  coveredPrefixHash?: string;
  inputHash?: string;
  outputHash?: string;
  summaryHash?: string;
  archive?: string;
  blockedInputHash?: string;
  createdAt?: string;
}

export interface ContextMaintenanceInfo {
  canonicalTokens?: number;
  projectedTokens?: number;
  summaryTokens?: number;
  lastSavedTokens?: number;
  snipTrigger?: number;
  foldTrigger?: number;
  forceTrigger?: number;
  hardInputCeiling?: number;
  headroom?: number;
  projectionVersion?: number;
  blocked?: boolean;
  lastReceipt?: ContextMaintenanceReceipt;
}
