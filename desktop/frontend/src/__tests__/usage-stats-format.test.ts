// Run: tsx src/__tests__/usage-stats-format.test.ts

import { formatUsageTokens } from "../lib/usageStatsFormat";

let failed = 0;

function ok(value: boolean, label: string) {
  if (value) {
    process.stdout.write(`  PASS  ${label}\n`);
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

const english = formatUsageTokens(10_000, "en");
const chinese = formatUsageTokens(10_000, "zh");

ok(!english.includes("万") && !english.includes("亿"), "English token totals do not use Chinese units");
ok(chinese !== english, "token totals follow the selected desktop locale");

if (failed > 0) process.exit(1);
