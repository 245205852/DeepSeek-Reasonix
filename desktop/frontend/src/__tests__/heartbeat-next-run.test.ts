// Run: tsx src/__tests__/heartbeat-next-run.test.ts

import { cronToInterval, heartbeatBuildCycleInterval, heartbeatNextRunAt, intervalToCron, nextCycleRunAt } from "../custom/features/heartbeat/HeartbeatPanel";

let passed = 0;
let failed = 0;

function eq(a: unknown, b: unknown, label: string) {
  if (a === b) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}: expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}\n`);
    failed += 1;
  }
}

function localMs(year: number, month: number, day: number, hour: number, minute: number): number {
  return new Date(year, month - 1, day, hour, minute, 0, 0).getTime();
}

console.log("\nheartbeat next run");

eq(
  heartbeatNextRunAt(
    { interval: "30m", lastRunAt: localMs(2026, 6, 18, 16, 30) },
    localMs(2026, 6, 18, 17, 20),
  ),
  localMs(2026, 6, 18, 17, 0),
  "plain interval stays due after elapsed",
);

eq(
  heartbeatNextRunAt(
    { interval: "30m", lastRunAt: localMs(2026, 6, 18, 16, 30), timeWindowStart: "09:00", timeWindowEnd: "17:00" },
    localMs(2026, 6, 18, 17, 20),
  ),
  localMs(2026, 6, 19, 9, 0),
  "time window defers elapsed interval to next opening",
);

eq(
  heartbeatNextRunAt(
    { interval: "30m", lastRunAt: localMs(2026, 6, 18, 16, 0), timeWindowStart: "09:00", timeWindowEnd: "17:00" },
    localMs(2026, 6, 18, 16, 10),
  ),
  localMs(2026, 6, 18, 16, 30),
  "time window keeps next run inside the open window",
);

eq(
  heartbeatNextRunAt(
    { interval: "30m", lastRunAt: localMs(2026, 6, 18, 21, 50), timeWindowStart: "22:00", timeWindowEnd: "06:00" },
    localMs(2026, 6, 18, 22, 10),
  ),
  localMs(2026, 6, 18, 22, 20),
  "cross-midnight window keeps due time in the open window",
);

eq(
  heartbeatNextRunAt(
    { interval: "30m", lastRunAt: localMs(2026, 6, 18, 11, 30), timeWindowStart: "22:00", timeWindowEnd: "06:00" },
    localMs(2026, 6, 18, 12, 10),
  ),
  localMs(2026, 6, 18, 22, 0),
  "cross-midnight window waits for today's opening from midday",
);

eq(
  heartbeatNextRunAt(
    { interval: "24h|daily@20:00", lastRunAt: localMs(2026, 6, 18, 20, 0), timeWindowStart: "09:00", timeWindowEnd: "17:00" },
    localMs(2026, 6, 19, 19, 0),
  ),
  localMs(2026, 6, 19, 20, 0),
  "cycle next run ignores stale interval time windows",
);

eq(
  heartbeatBuildCycleInterval("daily", [], "09:00"),
  "24h|weekly:mon@09:00",
  "empty daily day selection does not save as every day",
);

eq(
  heartbeatBuildCycleInterval("weekly", [], "09:00"),
  "168h|weekly:mon@09:00",
  "weekly default uses one weekday",
);

eq(
  heartbeatNextRunAt(
    { interval: "0 9 * * 1-5", lastRunAt: 0 },
    localMs(2026, 6, 15, 10, 0),
  ),
  localMs(2026, 6, 16, 9, 0),
  "cron weekday schedule computes next business-day run",
);

eq(
  heartbeatNextRunAt(
    { interval: "*/15 * * * *", lastRunAt: 0 },
    localMs(2026, 6, 18, 10, 7),
  ),
  localMs(2026, 6, 18, 10, 15),
  "cron every-15-minutes computes next slot",
);

eq(
  heartbeatNextRunAt(
    { interval: "0 9 * * 1-5", lastRunAt: localMs(2026, 6, 15, 9, 0) },
    localMs(2026, 6, 15, 9, 0),
  ),
  localMs(2026, 6, 16, 9, 0),
  "cron next run ignores lastRunAt and uses wall clock",
);

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);


// ── 周期转 cron / 周期 next-run 语义（SivanCola review 回归） ──

console.log("cycle → cron conversion guards");

// biweekly 无法无损转 cron → null
eq(intervalToCron("336h|biweekly:mon@09:00"), null, "biweekly refuses cron conversion");

// 秒级任务无法转 cron → null
eq(intervalToCron("30s"), null, "seconds refuse cron conversion");

// 跨午夜窗口 22:00–06:00 无法表达 → null
eq(intervalToCron("1h", "22:00", "06:00"), null, "cross-midnight window refuses conversion");

// 时间窗口结束时刻 exclusive：09:00–17:00 → 小时 9-16（不含 17 点）
eq(intervalToCron("1h", "09:00", "17:00"), "0 9-16 * * *", "window end hour is exclusive");

// daily 周期正常转换（无窗口）
eq(intervalToCron("24h|daily@09:00"), "0 9 * * *", "daily converts to cron");

// weekly 周期正常转换
eq(intervalToCron("168h|weekly:mon,wed@09:00"), "0 9 * * 1,3", "weekly converts to cron");

// monthly 周期正常转换
eq(intervalToCron("720h|monthly:15@09:00"), "0 9 15 * *", "monthly converts to cron");

console.log("cycle next-run (schedule semantics, not cron)");

// daily: 下个 22:00
eq(
  nextCycleRunAt("24h|daily@22:00", localMs(2026, 8, 11, 10, 0)),
  localMs(2026, 8, 11, 22, 0),
  "daily next run is today's 22:00",
);

// daily: 已过 22:00 → 明天 22:00
eq(
  nextCycleRunAt("24h|daily@22:00", localMs(2026, 8, 11, 23, 0)),
  localMs(2026, 8, 12, 22, 0),
  "daily next run rolls to tomorrow after 22:00",
);

// weekly: 下个周五 16:00（2026-08-14 是周五）
eq(
  nextCycleRunAt("168h|weekly:fri@16:00", localMs(2026, 8, 11, 10, 0)),
  localMs(2026, 8, 14, 16, 0),
  "weekly next run is next Friday 16:00",
);

// monthly: 下个 15 号 09:00
eq(
  nextCycleRunAt("720h|monthly:15@09:00", localMs(2026, 8, 11, 10, 0)),
  localMs(2026, 8, 15, 9, 0),
  "monthly next run is 15th 09:00",
);

// yearly: 下个 1-1 09:00（2027-01-01）
eq(
  nextCycleRunAt("8760h|yearly:1-1@09:00", localMs(2026, 8, 11, 10, 0)),
  localMs(2027, 1, 1, 9, 0),
  "yearly next run is next Jan 1st 09:00",
);

// ── review fixes 回归 ──

console.log("cron dow=7 Sunday alias");

eq(
  heartbeatNextRunAt(
    { interval: "0 9 * * 7", lastRunAt: 0 },
    localMs(2026, 8, 10, 10, 0), // Monday
  ),
  localMs(2026, 8, 16, 9, 0), // next Sunday 09:00
  "dow=7 (Sunday alias) computes the next Sunday run",
);

eq(
  heartbeatNextRunAt(
    { interval: "0 9 * * 0,7", lastRunAt: 0 },
    localMs(2026, 8, 10, 10, 0), // Monday
  ),
  localMs(2026, 8, 16, 9, 0),
  "dow=0,7 both spellings compute the next Sunday run",
);

console.log("biweekly parity mirrors the Go engine (Monday week anchor)");

// 2026-08-06 is a Thursday; engine parity is anchored on the Monday of the
// creation week (2026-08-03). Candidate Monday 2026-08-10 is week 1 away →
// skipped; next fire is Monday 2026-08-17 (week 2).
eq(
  nextCycleRunAt("336h|biweekly:mon@09:00", localMs(2026, 8, 10, 10, 0), localMs(2026, 8, 6, 9, 0)),
  localMs(2026, 8, 17, 9, 0),
  "biweekly with Thursday anchor skips the first Monday (Monday-anchored parity)",
);

// Anchor on a Monday itself: creation week = 2026-08-10; next fire is 2026-08-24.
eq(
  nextCycleRunAt("336h|biweekly:mon@09:00", localMs(2026, 8, 10, 10, 0), localMs(2026, 8, 10, 9, 0)),
  localMs(2026, 8, 24, 9, 0),
  "biweekly with Monday anchor fires every other Monday",
);

console.log("cronToInterval refuses lossy conversions");

eq(cronToInterval("0 9 * * 1"), null, "weekly cron refuses interval conversion");
eq(cronToInterval("0 9 * * *"), null, "fixed-time cron refuses interval conversion");
eq(cronToInterval("0 0 1 * *"), null, "dom-restricted cron refuses interval conversion");
eq(cronToInterval("*/15 * * * *"), "15m", "every-N-minutes cron converts");
eq(cronToInterval("0 */2 * * *"), "2h", "every-N-hours cron converts");
eq(cronToInterval("0 * * * *"), null, "top-of-every-hour is not a simple interval");

console.log("frontend isCronExpr field bounds (dom/month 1-based)");

eq(
  heartbeatNextRunAt(
    { interval: "0 0 0 * *", lastRunAt: 0 }, // dom=0 — "midnight every day" typo
    localMs(2026, 8, 10, 10, 0),
  ),
  null,
  "dom=0 is rejected: no next run is computed for an invalid expression",
);

eq(
  heartbeatNextRunAt(
    { interval: "0 0 1 0 *", lastRunAt: 0 }, // month=0
    localMs(2026, 8, 10, 10, 0),
  ),
  null,
  "month=0 is rejected",
);

eq(
  heartbeatNextRunAt(
    { interval: "0 0 1 * *", lastRunAt: 0 },
    localMs(2026, 8, 10, 10, 0),
  ),
  localMs(2026, 9, 1, 0, 0),
  "valid dom expression still computes a next run",
);
