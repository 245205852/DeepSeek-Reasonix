import type { DictKey, Translator } from "./i18n";

export type CompletionSummaryTone = "complete" | "partial" | "blocked" | "continue";

export interface CompletionSummaryDisplayInput {
  preset: string;
  verdict: string;
  mutations: number;
  checksPassed: number;
  checksFailed: number;
  checksSuppressed: number;
  review: string;
  gapKinds?: string[];
  constraintDegraded: boolean;
}

export interface CompletionSummaryDisplay {
  tone: CompletionSummaryTone;
  title: string;
  verdict: string;
  preset: string;
  metrics: string[];
  details: string[];
}

const PRESET_KEYS: Record<string, DictKey> = {
  light: "composer.runtimeProfileEconomyShort",
  balanced: "composer.runtimeProfileBalancedShort",
  delivery: "composer.runtimeProfileDeliveryShort",
};
const VERDICT_KEYS: Record<CompletionSummaryTone, DictKey> = {
  complete: "notice.completionVerdictComplete",
  partial: "notice.completionVerdictPartial",
  blocked: "notice.completionVerdictBlocked",
  continue: "notice.completionVerdictContinue",
};
const REVIEW_KEYS: Record<string, DictKey> = {
  passed: "notice.completionReviewPassed",
  unavailable: "notice.completionReviewUnavailable",
};
const GAP_KEYS: Record<string, DictKey> = {
  suppressed: "notice.completionGapSuppressed",
  stale_check: "notice.completionGapStaleCheck",
  suppressed_requirement: "notice.completionGapSuppressedRequirement",
};

export function completionSummaryDisplay(item: CompletionSummaryDisplayInput, t: Translator): CompletionSummaryDisplay {
  const tone: CompletionSummaryTone = item.verdict === "complete"
    || item.verdict === "partial"
    || item.verdict === "blocked"
    || item.verdict === "continue"
    ? item.verdict as CompletionSummaryTone
    : "continue";
  const metrics: string[] = [];
  for (const [count, key] of [
    [item.mutations, "notice.completionMutations"],
    [item.checksPassed, "notice.completionChecksPassed"],
    [item.checksFailed, "notice.completionChecksFailed"],
    [item.checksSuppressed, "notice.completionChecksSuppressed"],
  ] as const) {
    if (Number.isFinite(count) && count > 0) metrics.push(t(key, { n: Math.floor(count) }));
  }

  const details: string[] = [];
  if (item.review && item.review !== "none") {
    details.push(t(REVIEW_KEYS[item.review] ?? "notice.completionReviewUnavailable"));
  }
  for (const gap of item.gapKinds ?? []) {
    const label = t(GAP_KEYS[gap] ?? "notice.completionGapUnknown");
    if (!details.includes(label)) details.push(label);
  }
  if (item.constraintDegraded) {
    const label = t("notice.completionConstraintDegraded");
    if (!details.includes(label)) details.push(label);
  }

  return {
    tone,
    title: t("notice.completionSummaryTitle"),
    verdict: t(VERDICT_KEYS[tone]),
    preset: t(PRESET_KEYS[item.preset] ?? "composer.runtimeProfileTitle"),
    metrics,
    details,
  };
}
