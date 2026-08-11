import assert from "node:assert/strict";
import { completionSummaryDisplay, type CompletionSummaryDisplayInput } from "../lib/completionSummary";
import type { DictKey, Translator } from "../lib/i18n";
import { en } from "../locales/en";
import { zh } from "../locales/zh";

function translator(dict: Record<DictKey, string>): Translator {
  return (key, vars) => {
    const value = dict[key];
    if (!vars) return value;
    return value.replace(/\{(\w+)\}/g, (_, name: string) => (
      vars[name] === undefined ? `{${name}}` : String(vars[name])
    ));
  };
}

const partial: CompletionSummaryDisplayInput = {
  preset: "balanced",
  verdict: "partial",
  mutations: 3,
  checksPassed: 12,
  checksFailed: 0,
  checksSuppressed: 2,
  review: "passed",
  gapKinds: ["suppressed", "stale_check", "mystery_gap", "mystery_gap"],
  constraintDegraded: true,
};

const zhDisplay = completionSummaryDisplay(partial, translator(zh));
assert.equal(zhDisplay.title, "本轮执行结果");
assert.equal(zhDisplay.verdict, "部分完成");
assert.equal(zhDisplay.preset, "均衡");
assert.deepEqual(zhDisplay.metrics, ["3 项变更", "12 项检查通过", "2 项检查跳过"]);
assert.deepEqual(zhDisplay.details, [
  "独立复审通过",
  "部分检查被跳过",
  "检查结果已失效",
  "仍有其他验证缺口",
  "本轮部分验证受限",
]);
assert.ok(!JSON.stringify(zhDisplay).includes("stale_check"), "wire gap identifiers stay out of localized UI");

const unknownDisplay = completionSummaryDisplay({
  ...partial,
  preset: "future-preset",
  verdict: "future-verdict",
  mutations: Number.NaN,
  checksPassed: -1,
  checksFailed: 0,
  checksSuppressed: 0,
  review: "future-review",
  gapKinds: ["future-gap"],
  constraintDegraded: false,
}, translator(en));
assert.equal(unknownDisplay.tone, "continue");
assert.equal(unknownDisplay.preset, "Execution setting");
assert.equal(unknownDisplay.verdict, "Needs more work");
assert.deepEqual(unknownDisplay.metrics, [], "zero, negative, and non-finite counts stay hidden");
assert.deepEqual(unknownDisplay.details, ["Independent review incomplete", "Other verification gaps remain"]);
assert.ok(!JSON.stringify(unknownDisplay).includes("future-"), "unknown wire identifiers use safe display fallbacks");

console.log("completion summary display tests passed");
