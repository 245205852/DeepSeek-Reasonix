// Heartbeat Panel — Modal for configuring scheduled heartbeat tasks.
//
// Renders a list of tasks with add/edit/delete controls, plus a manual
// "run now" button for each. The panel is opened from the sidebar nav item.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import {
  Activity,
  Check,
  ChevronDown,
  ChevronRight,
  ChevronsUpDown,
  Circle,
  CirclePause,
  FolderTree,
  Heart,
  Lightbulb,
  List,
  MessageSquare,
  MoreHorizontal,
  Play,
  Plus,
  Search,
  Trash2,
  X,
} from "lucide-react";
import { app } from "../../../lib/bridge";
import { useT, type Translator } from "../../../lib/i18n";
import { AnchoredPopover } from "../../../components/AnchoredPopover";
import { Tooltip } from "../../../components/Tooltip";
import {
  heartbeatListTasks,
  heartbeatSaveTasks,
  heartbeatTriggerNow,
  heartbeatGenerateID,
} from "./heartbeat.bridge";
import type { HeartbeatTask } from "./heartbeat.types";
import type { WorkspaceView } from "../../../lib/types";
// 样式跟着组件走：App.tsx 通过 React.lazy 按需加载本组件，CSS 在此动态
// 静态导入：Vite 保证 CSS 在模块 evaluate 前注入 DOM，避免首次访问自动化页
// 无样式闪烁（FOUC）。node 单测通过 css-stub-register.mjs 的 loader hook 解析。
import "./heartbeat.css";
// 日历调度计算统一收敛在 heartbeat.schedule.ts（月末 clamp/闰日/双周锚点/
// DST 一次实现，前后端语义镜像），面板只做 cron 分支与展示格式化。
import { heartbeatNextRunAt as calendarHeartbeatNextRunAt, parseCalendarSchedule, nextCalendarRunImpl } from "./heartbeat.schedule";

interface HeartbeatPanelProps {
  onOpenTopic?: (scope: string, workspaceRoot: string, topicId: string) => void;
}

// mergeEngineRunState 只合并引擎拥有的运行状态字段（runHistory/topicId/lastRunAt）
// 到当前编辑快照，保留用户在等待期间对 title/prompt/interval 等草稿字段的未保存
// 编辑——"立即运行"完成后磁盘旧快照不得覆盖新输入。fresh 缺失的字段不覆盖
// 草稿中已有的引擎状态（如磁盘旧版无 runHistory 时保留草稿里的执行历史）。
export function mergeEngineRunState(prev: HeartbeatTask, fresh: HeartbeatTask): HeartbeatTask {
  return {
    ...prev,
    ...(fresh.runHistory ? { runHistory: fresh.runHistory } : {}),
    ...(fresh.topicId ? { topicId: fresh.topicId } : {}),
    ...(fresh.lastRunAt ? { lastRunAt: fresh.lastRunAt } : {}),
  };
}

// 圆圈内实心播放三角（停用态图标——对齐 ChatGPT 暂停态样式；strokeWidth 2.4 ≈ 15px 下 1.5px，与空圆圈 border 一致）
function CirclePlaySolid({ size = 15 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2.4}
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="10" />
      <path d="M10 8l6 4-6 4z" fill="currentColor" stroke="none" />
    </svg>
  );
}

// 日历调度计算（interval 窗口 / daily / weekly / biweekly / monthly / yearly /
// 月末 clamp / 闰日 / 双周锚点 / DST）统一委托给 heartbeat.schedule.ts，面板只
// 负责 cron 分支与展示格式化，避免两套日历逻辑漂移。
export function heartbeatNextRunAt(task: Pick<HeartbeatTask, "interval" | "lastRunAt" | "createdAt" | "timeWindowStart" | "timeWindowEnd">, now = Date.now()): number | null {
  if (isCronExpr(task.interval || "")) {
    return nextCronRunAt(task.interval || "", now);
  }
  return calendarHeartbeatNextRunAt(task, now);
}

// 周期任务（"24h|daily@22:00" 等）从 `from` 起的下一次触发时刻。calendar
// schedule 的 nextCalendarRun 语义就是 "after 之后的下一次"，与后端
// previousHeartbeatScheduleAt 对齐（月末 clamp、闰日、周一制双周锚点）。
export function nextCycleRunAt(interval: string, from = Date.now(), createdAt?: number): number | null {
  const schedule = parseCalendarSchedule(interval);
  if (!schedule) return null;
  const after = new Date(from);
  const anchor = createdAt ? new Date(createdAt) : after;
  return nextCalendarRunImpl(schedule, after, anchor).getTime();
}

// 列表排序：启用在前、禁用在后；同组按下次触发时间升序（越近越靠前，未运行视为立即）。
function sortTasksByNextRun(a: HeartbeatTask, b: HeartbeatTask): number {
  if (a.enabled !== b.enabled) return a.enabled ? -1 : 1;
  const now = Date.now();
  const nr = (t: HeartbeatTask) => (t.enabled && !t.lastRunAt ? now : heartbeatNextRunAt(t, now)) ?? Number.POSITIVE_INFINITY;
  return nr(a) - nr(b);
}

function formatInterval(interval: string, t: Translator): string {
  const cycleMatch = interval.match(/^(\d+)[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
  if (cycleMatch) {
    const [, , type, days, time] = cycleMatch;
    // 格式参考：周一（时间：9:00）；每天直接「每天 22:00」，不套（时间：）包装
    const timeStr = time ? t("heartbeat.cycleTimeAt", { time }) : "";
    const dayNames = (d: string) => {
      const wd = WEEKDAYS.find((w) => w.key === d);
      return wd ? t(wd.labelKey) : d;
    };
    if (type === "daily") return time ? `${t("heartbeat.cycleDaily")} ${time}` : t("heartbeat.cycleDaily");
    if (type === "weekly") {
      const list = (days || "").split(",").filter(Boolean).map(dayNames).join(t("heartbeat.joinComma"));
      return `${list || t("heartbeat.cycleWeekly")}${timeStr}`;
    }
    if (type === "biweekly") {
      const list = (days || "").split(",").filter(Boolean).map(dayNames).join(t("heartbeat.joinComma"));
      return `${t("heartbeat.cycleBiweekly")}${list ? ` ${list}` : ""}${timeStr}`;
    }
    if (type === "monthly") return `${t("heartbeat.cycleMonthly")}${days ? ` ${days}${t("heartbeat.monthDay")}` : ""}${timeStr}`;
    if (type === "yearly") {
      const parts = (days || "").split("-");
      return `${t("heartbeat.cycleYearly")} ${parts[0] || "1"}/${parts[1] || "1"}${timeStr}`;
    }
  }
  const simple = interval.match(/^(\d+)([smh])$/);
  if (simple) {
    const unitLabels: Record<string, string> = {
      s: t("heartbeat.unitSec"),
      m: t("heartbeat.unitMin"),
      h: t("heartbeat.unitHour"),
    };
    return `${t("heartbeat.freqEvery")} ${simple[1]}${unitLabels[simple[2]] || simple[2]}`;
  }
  return interval;
}

// ── Cron expressions ─────────────────────────────────────────────────────────

// isCronExpr returns true when s looks like a 5-field cron expression
// (e.g. "0 * * * *", "*/15 * * * *", "0 9 * * 1-5").
// formatRelativeTime renders how long ago something happened: "just now",
// "N minutes ago", "N hours ago", "N days ago".
function formatRelativeTime(at: number, now: number, t: Translator): string {
  const diff = Math.max(0, now - at);
  const min = Math.floor(diff / 60000);
  if (min < 1) return t("heartbeat.justNow");
  if (min < 60) return t("heartbeat.minutesAgo", { n: min });
  const hr = Math.floor(min / 60);
  if (hr < 24) return t("heartbeat.hoursAgo", { n: hr });
  const d = Math.floor(hr / 24);
  return t("heartbeat.daysAgo", { n: d });
}

function isCronExpr(s: string): boolean {
  const fields = s.trim().split(/\s+/);
  if (fields.length !== 5) return false;
  if (!fields.every((f) => f !== "" && /^[0-9*/\-,]+$/.test(f))) return false;
  // Reject out-of-range values (e.g. "99 * * * *"), zero/empty steps
  // ("*/0" never fires: value % 0 is NaN), and descending ranges ("5-1"
  // never matches). dom/month are 1-based (0 can never match getDate()/
  // getMonth()); dow is 0-7 with 7 accepted as the Sunday alias — mirror the
  // Go engine exactly.
  const limits = [59, 23, 31, 12, 7]; // min, hour, dom, month, dow
  const mins = [0, 0, 1, 1, 0];
  return fields.every((f, i) =>
    f.split(",").every((part) => {
      const slashIdx = part.indexOf("/");
      const base = slashIdx >= 0 ? part.slice(0, slashIdx) : part;
      if (slashIdx >= 0) {
        const step = Number(part.slice(slashIdx + 1));
        if (!Number.isInteger(step) || step < 1) return false;
      }
      if (base === "*") return true;
      if (base.includes("-")) {
        const [lo, hi] = base.split("-").map(Number);
        return Number.isInteger(lo) && Number.isInteger(hi)
          && lo >= mins[i] && hi <= limits[i] && lo <= hi;
      }
      const v = Number(base);
      return Number.isInteger(v) && v >= mins[i] && v <= limits[i];
    })
  );
}

function cronFieldMatch(pattern: string, value: number, minValue: number, maxValue: number): boolean {
  for (const part of pattern.split(",")) {
    const p = part.trim();
    let base = p;
    let step = 1;
    const slashIdx = p.indexOf("/");
    if (slashIdx >= 0) {
      base = p.slice(0, slashIdx);
      step = parseInt(p.slice(slashIdx + 1)) || 1;
    }
    if (step <= 0) continue;
    if (base === "*") {
      if (value >= minValue && value <= maxValue && (value - minValue) % step === 0) return true;
      continue;
    }
    const dashIdx = base.indexOf("-");
    let low = parseInt(base);
    let high = low;
    if (dashIdx >= 0) {
      low = parseInt(base.slice(0, dashIdx));
      high = parseInt(base.slice(dashIdx + 1));
    } else if (slashIdx >= 0) {
      high = maxValue;
    }
    if (isNaN(low) || isNaN(high)) continue;
    if (value < low || value > high) continue;
    if ((value - low) % step === 0) return true;
  }
  return false;
}

// nextCronRunAt returns the timestamp of the next time the 5-field cron
// expression matches, starting from `from` (default now). Returns null when
// the expression is invalid or nothing matches within the search horizon.
function nextCronRunAt(expr: string, from = Date.now()): number | null {
  if (!isCronExpr(expr)) return null;
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  const [minP, hourP, domP, monP, dowP] = fields;
  if (![minP, hourP, domP, monP, dowP].every((f) => /^[0-9*/\-,]+$/.test(f))) return null;
  const base = new Date(from);
  base.setSeconds(0, 0);
  for (let day = 0; day <= 366 * 8; day++) {
    const d = new Date(base);
    d.setDate(d.getDate() + day);
    if (!cronFieldMatch(monP, d.getMonth() + 1, 1, 12)) continue;
    // Standard cron: dom & dow are OR-ed when both are restricted.
    const domRestricted = domP !== "*";
    const dowRestricted = dowP !== "*";
    const domMatch = cronFieldMatch(domP, d.getDate(), 1, 31);
    // 7 is the standard Sunday alias in the dow field (getDay() is 0-6).
    const dowMatch = cronFieldMatch(dowP, d.getDay(), 0, 7) || (d.getDay() === 0 && cronFieldMatch(dowP, 7, 0, 7));
    const dayMatch = domRestricted && dowRestricted ? domMatch || dowMatch
      : domRestricted ? domMatch
      : dowRestricted ? dowMatch
      : true;
    if (!dayMatch) continue;
    const hStart = day === 0 ? d.getHours() : 0;
    for (let h = hStart; h < 24; h++) {
      if (!cronFieldMatch(hourP, h, 0, 23)) continue;
      const mStart = day === 0 && h === hStart ? d.getMinutes() + 1 : 0;
      for (let m = mStart; m < 60; m++) {
        if (!cronFieldMatch(minP, m, 0, 59)) continue;
        return new Date(d.getFullYear(), d.getMonth(), d.getDate(), h, m, 0, 0).getTime();
      }
    }
  }
  return null;
}

function formatCronNext(ts: number | null): string {
  if (ts === null) return "";
  const d = new Date(ts);
  return `${(d.getMonth() + 1).toString().padStart(2, "0")}/${d.getDate().toString().padStart(2, "0")} ${d.getHours().toString().padStart(2, "0")}:${d.getMinutes().toString().padStart(2, "0")}`;
}

// intervalToCron converts a cycle ("24h|daily@09:00") or simple ("30m", "1h")
// interval into a 5-field cron expression. Already-cron values pass through.
export function intervalToCron(interval: string, timeWindowStart?: string, timeWindowEnd?: string): string | null {
  // Guard: biweekly cannot be losslessly expressed in 5-field cron (DOM/DOW
  // are OR-ed, so a biweekly rule like "1-15 * 1" becomes "1st-15th OR Monday",
  // doubling the actual frequency). Seconds cannot be expressed either (cron
  // has no seconds field). Cross-midnight windows (22:00–06:00) produce a
  // descending range "22-6" that no matcher handles. Non-top-of-hour windows
  // (09:30–17:30) would be truncated to whole hours. Return null for these so
  // callers can refuse the conversion instead of silently corrupting semantics.
  const windowTopOfHour = !timeWindowStart && !timeWindowEnd
    || (!timeWindowStart || timeWindowStart.endsWith(":00"))
    && (!timeWindowEnd || timeWindowEnd.endsWith(":00"));
  if (!windowTopOfHour) return null;
  const cycleMatch = interval.match(/^\d+[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
  if (cycleMatch) {
    const kind = cycleMatch[1];
    if (kind === "biweekly") return null;
    const days = cycleMatch[2] || "";
    const time = cycleMatch[3] || "09:00";
    const [h, m] = time.split(":").map(Number);
    // Cycle tasks schedule on their own clock (@09:00 etc); the engine ignores
    // interval-style time windows for them (see heartbeatTaskDueAt), so any
    // stale window must not be folded into the cron hour field — that would
    // turn "daily@12:00" into "every hour 9-16".
    const dayMap: Record<string, number> = { mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6, sun: 0 };
    switch (kind) {
      case "daily": return `${m} ${h} * * *`;
      case "weekly": {
        const d = days.split(",").map((x) => dayMap[x.toLowerCase()] ?? "*").join(",");
        return `${m} ${h} * * ${d}`;
      }
      case "monthly": return `${m} ${h} ${days || "1"} * *`;
      case "yearly": {
        const [mo, dy] = days.split("-");
        return `${m} ${h} ${dy || "1"} ${mo || "1"} *`;
      }
    }
  }
  const simple = interval.match(/^(\d+)([smh])$/);
  if (simple) {
    const n = parseInt(simple[1]);
    const unit = simple[2];
    if (timeWindowStart && timeWindowEnd
      && parseInt(timeWindowStart.split(":")[0]) > parseInt(timeWindowEnd.split(":")[0])) {
      return null; // cross-midnight window: not expressible
    }
    const hExpr = timeWindowStart && timeWindowEnd
      // End is exclusive: "09:00–17:00" → hour range 9-16 (17:00 excluded).
      ? `${Math.max(0, parseInt(timeWindowStart.split(":")[0]))}-${Math.max(0, Math.min(23, parseInt(timeWindowEnd.split(":")[0]) - 1))}`
      : "*";
    if (unit === "m") return `*/${n} ${hExpr} * * *`;
    // 5-field cron: minute hour dom mon dow. Hourly tasks run at minute 0 of
    // every n-th hour (`0 */n * * *`). With a time window the hour field can
    // only carry "all hours in the range" (`0 9-16 * * *`), which is lossless
    // for 1h but would silently change 2h+ windows from "every N hours" to
    // "every hour" — refuse those.
    if (unit === "h") {
      if (timeWindowStart && timeWindowEnd && n > 1) return null;
      const hourField = timeWindowStart && timeWindowEnd ? hExpr : `*/${n}`;
      return `0 ${hourField} * * *`;
    }
    if (unit === "s") return null; // seconds cannot be expressed in cron
  }
  if (isCronExpr(interval)) return interval.trim();
  return null;
}

// cronToInterval reverse-converts a cron expression back to a simple interval.
// Returns null when the expression cannot be expressed as a plain "every N
// minutes/hours" interval without changing semantics (dom/dow/month-restricted
// or fixed-time schedules) — callers must keep the cron instead of silently
// rewriting e.g. a weekly "0 9 * * 1" into "1h".
export function cronToInterval(cron: string): string | null {
  const f = cron.trim().split(/\s+/);
  if (f.length !== 5) return null;
  // Only pure every-N minute/hour schedules round-trip losslessly.
  if (f[2] !== "*" || f[3] !== "*" || f[4] !== "*") return null;
  const min = f[0], hour = f[1];
  if (min.startsWith("*/") && hour === "*") return `${min.slice(2)}m`;
  if (min === "0" && hour.startsWith("*/")) return `${hour.slice(2)}h`;
  return null;
}

export type HeartbeatFrequencyType = "interval" | "daily" | "weekly" | "biweekly" | "monthly" | "yearly" | "cron";

export function changeHeartbeatFrequency(task: HeartbeatTask, frequency: HeartbeatFrequencyType): HeartbeatTask | null {
  const current = task.interval || "";
  if (frequency === "daily") return { ...task, interval: "24h|daily:mon,tue,wed,thu,fri,sat,sun@09:00" };
  if (frequency === "weekly") return { ...task, interval: "168h|weekly:mon@09:00" };
  if (frequency === "biweekly") return { ...task, interval: "336h|biweekly:mon@09:00" };
  if (frequency === "monthly") return { ...task, interval: "720h|monthly:1@09:00" };
  if (frequency === "yearly") return { ...task, interval: "8760h|yearly:1-1@09:00" };
  if (frequency === "cron") {
    if (isCronExpr(current)) return task;
    const converted = intervalToCron(current, task.timeWindowStart, task.timeWindowEnd);
    return converted === null ? null : { ...task, interval: converted };
  }
  if (isCronExpr(current)) {
    const converted = cronToInterval(current);
    return converted === null ? null : { ...task, interval: converted };
  }
  if (current.includes("|")) return { ...task, interval: current.replace(/\|.*$/, "") };
  return task;
}

// describeCron renders a human-readable description of a 5-field cron
// expression, localized via t().
function describeCron(expr: string, t: Translator): string {
  const f = expr.trim().split(/\s+/);
  if (f.length !== 5) return "";
  const min = f[0], hour = f[1], dom = f[2], mon = f[3], dow = f[4];

  const hourRange = (h: string): string => {
    if (!h || h === "*") return "";
    if (h.includes("/")) {
      const base = h.split("/")[0];
      if (base.includes("-")) {
        const parts = base.split("-");
        return `${parts[0].padStart(2, "0")}:00-${parts[1].padStart(2, "0")}:00`;
      }
      return "";
    }
    if (h.includes("-")) {
      const parts = h.split("-");
      return `${parts[0].padStart(2, "0")}:00-${parts[1].padStart(2, "0")}:00`;
    }
    return "";
  };
  const wd = hourRange(hour);

  if (min.startsWith("*/") && hour !== "*" && hour.includes("-")) {
    return `${t("heartbeat.cronEveryMin", { n: min.slice(2) })} (${wd})`;
  }
  if (min.startsWith("*/") && hour === "*") return t("heartbeat.cronEveryMin", { n: min.slice(2) });
  if (min.startsWith("*/") && hour !== "*") return `${t("heartbeat.cronEveryMin", { n: min.slice(2) })} ${wd}`;
  if (min === "0" && hour !== "*" && dom === "*" && mon === "*" && dow === "*") {
    if (hour.includes("/")) return t("heartbeat.cronEveryHour", { n: hour.replace("*/", "") });
    if (hour.includes("-")) return `${t("heartbeat.cronHourly")} (${wd})`;
    return t("heartbeat.cronAt", { time: `${hour.padStart(2, "0")}:00` });
  }
  if (min === "0" && hour === "*" && dom === "*" && mon === "*" && dow === "*") return t("heartbeat.cronHourly");
  if (min !== "*" && !min.includes("/") && hour === "*" && dom === "*" && mon === "*" && dow === "*") {
    return t("heartbeat.cronOnHour", { n: min });
  }
  if (dow !== "*" && dow !== "") {
    const weekdays: Record<string, string> = {
      "0": t("heartbeat.cronWeekdaySun"), "1": t("heartbeat.cronWeekdayMon"),
      "2": t("heartbeat.cronWeekdayTue"), "3": t("heartbeat.cronWeekdayWed"),
      "4": t("heartbeat.cronWeekdayThu"), "5": t("heartbeat.cronWeekdayFri"),
      "6": t("heartbeat.cronWeekdaySat"), "7": t("heartbeat.cronWeekdaySun"),
    };
    const days = dow.split(",").map((d) => weekdays[d] || d).join(t("heartbeat.joinComma"));
    const suffix = wd ? ` (${wd})` : "";
    return `${days} ${hour.padStart(2, "0")}:${min.padStart(2, "0")}${suffix}`;
  }
  const suffix = wd ? ` (${wd})` : "";
  return `${hour.padStart(2, "0")}:${min.padStart(2, "0")}${suffix}`;
}

// 周期 next-run 计算统一在文件头部通过 heartbeat.schedule.ts 的
// parseCalendarSchedule + nextCalendarRunImpl 实现（见 nextCycleRunAt），
// 与新实现的意图一致：镜像后端 previousHeartbeatScheduleAt 语义、月末
// clamp、周一制双周锚点。此处不再保留旧的行内实现。

function taskNextRun(task: HeartbeatTask, t: Translator): string | null {
  if (!task.enabled) return null;
  const interval = task.interval || "";
  let next: number | null = null;
  // 周期任务（"24h|daily@22:00" / "168h|weekly:fri@16:00"）：按调度语义计算
  // 下一个匹配时刻。已运行过（有 lastRunAt）的任务基于 lastRunAt 求下一时刻
  // （heartbeatNextRunAt → heartbeat.schedule 的 nextCalendarRun，含月末
  // clamp/闰日/双周锚点/DST，与后端 previousHeartbeatScheduleAt 对齐）；离线
  // 期间早该运行的任务，next 会落在过去 → 显示 dueSoon（当前应执行），而不是
  // 跳到下一周期。从未运行的任务从创建时刻起算首次运行。
  const cycleMatch = interval.match(/^\d+[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
  if (cycleMatch) {
    next = task.lastRunAt
      ? heartbeatNextRunAt(task)
      : nextCycleRunAt(interval, task.createdAt || Date.now(), task.createdAt);
  } else {
    const cleaned = interval.replace(/\|.*$/, "");
    const m = cleaned.match(/^(\d+)([smh])$/);
    if (m) {
      // Plain interval with a time window: use the window-aware helper so the
      // displayed next run matches the backend (defers to the next opening
      // instead of naively showing lastRunAt + interval outside the window).
      if (task.timeWindowStart || task.timeWindowEnd) {
        next = heartbeatNextRunAt(task);
      } else {
        if (!task.lastRunAt) return null;
        const ms = parseInt(m[1]) * { s: 1000, m: 60000, h: 3600000 }[m[2] as "s" | "m" | "h"];
        next = task.lastRunAt + ms;
      }
    } else if (isCronExpr(cleaned)) {
      next = nextCronRunAt(cleaned);
    } else {
      return null;
    }
  }
  if (next === null) return null;
  if (next <= Date.now()) return t("heartbeat.dueSoon");
  const diff = next - Date.now();
  // 剩余时间：如「下次运行 26 分钟后」/「下次运行 2 小时后」/「下次运行 2天3小时后」/「即将触发」
  const days = Math.floor(diff / 86400000);
  const hours = Math.floor((diff % 86400000) / 3600000);
  const minutes = Math.floor((diff % 3600000) / 60000);
  const prefix = t("heartbeat.nextRun");
  if (days > 0) return `${prefix} ${days}${t("heartbeat.unitDay")}${hours}${t("heartbeat.unitHour")}${t("heartbeat.later")}`;
  if (hours > 0) return `${prefix} ${hours}${t("heartbeat.unitHour")}${minutes}${t("heartbeat.unitMin")}${t("heartbeat.later")}`;
  if (minutes > 0) return `${prefix} ${minutes}${t("heartbeat.unitMin")}${t("heartbeat.later")}`;
  return t("heartbeat.dueSoon");
}

export function HeartbeatView({ onOpenTopic }: HeartbeatPanelProps) {
  const t = useT();
  const [tasks, setTasks] = useState<HeartbeatTask[]>([]);
  const [loading, setLoading] = useState(false);
  const [editing, setEditing] = useState<HeartbeatTask | null>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<"all" | "enabled" | "disabled">("all");
  const [scopeFilter, setScopeFilter] = useState<string>("all");
  const [scopeFilterOpen, setScopeFilterOpen] = useState(false);
  const scopeFilterRef = useRef<HTMLButtonElement>(null);
  const [expandedProjects, setExpandedProjects] = useState<Set<string> | null>(null);
  const [workspaceMap, setWorkspaceMap] = useState<Record<string, string>>({});
  // Left list view: flat (default) or grouped by project scope.
  const [listView, setListView] = useState<"flat" | "grouped">("grouped");
  // Detail panel: hidden by default (the list fills the pane, like ChatGPT),
  // opens on task click with a 50/50 split and a draggable divider.
  const [detailOpen, setDetailOpen] = useState(false);
  // 分割线拖拽宽度持久化：localStorage 缓存上次拖拽比例（30-70），
  // 重新打开面板/切换视图后恢复，无需每次重拖。
  const [listWidthPct, setListWidthPct] = useState(() => {
    try {
      const cached = Number(localStorage.getItem("reasonix-heartbeat-list-width"));
      return Number.isFinite(cached) ? Math.min(70, Math.max(30, cached)) : 50;
    } catch {
      return 50;
    }
  });
  const dirtyRef = useRef(false);
  // IDs of drafts created via Add/scoped-Add/startNew that have not been saved yet.
  // They are intentionally absent from `tasks`, so the "clear editing" effect
  // below must not close the editor for them.
  const unsavedDraftIdsRef = useRef<Set<string>>(new Set());

  // ── 自定义滚动条：隐藏系统滚动条，DOM 直接驱动 + rAF 帧同步 ──
  const listRef = useRef<HTMLDivElement>(null);
  const thumbRef = useRef<HTMLDivElement>(null);
  const scrollbarRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const list = listRef.current;
    const thumb = thumbRef.current;
    const track = scrollbarRef.current;
    if (!list || !thumb || !track) return;

    // 滚动条自动隐藏：滚动/拖动时显示，停止 1.2s 后淡出
    let hideTimer = 0;
    const show = () => {
      track.classList.add("heartbeat-scrollbar--visible");
      window.clearTimeout(hideTimer);
      hideTimer = window.setTimeout(() => {
        track.classList.remove("heartbeat-scrollbar--visible");
      }, 1200);
    };

    const update = () => {
      const { scrollTop, scrollHeight, clientHeight } = list;
      if (scrollHeight <= clientHeight + 1) {
        thumb.style.display = "none";
        return;
      }
      thumb.style.display = "block";
      // 轨道（.heartbeat-scrollbar）覆盖整个 left 列，高度 ≠ 列表可视区；
      // thumb 尺寸/行程按轨道实际高度计算，保证滚到底时 thumb 也到底。
      const trackHeight = track.clientHeight;
      const height = Math.max(24, (clientHeight / scrollHeight) * trackHeight);
      const maxTop = trackHeight - height;
      const top = maxTop <= 0 ? 0 : (scrollTop / (scrollHeight - clientHeight)) * maxTop;
      thumb.style.height = `${Math.round(height)}px`;
      thumb.style.top = `${Math.round(top)}px`;
    };

    // rAF 节流：滚动/尺寸变化时在下一帧同步一次滑块位置
    let raf = 0;
    const schedule = () => {
      if (!raf) raf = requestAnimationFrame(() => { raf = 0; update(); });
    };
    // 滚动（含拖动 thumb 触发的 scroll）时显示滚动条并重置隐藏计时
    const onScroll = () => {
      show();
      schedule();
    };

    update();
    list.addEventListener("scroll", onScroll, { passive: true });
    const ro = new ResizeObserver(schedule);
    ro.observe(list);

    // 内容变化后 scrollHeight 更新，下一帧校准（含首次挂载）
    const t = window.setTimeout(update, 50);
    return () => {
      if (raf) cancelAnimationFrame(raf);
      window.clearTimeout(t);
      window.clearTimeout(hideTimer);
      list.removeEventListener("scroll", onScroll);
      ro.disconnect();
    };
  }, [tasks]);

  const onScrollThumbMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    const list = listRef.current;
    const track = scrollbarRef.current;
    if (!list || !track) return;
    // 拖动开始即显示滚动条，拖动过程中保持可见（scroll 事件会重置隐藏计时）
    track.classList.add("heartbeat-scrollbar--visible");
    const startY = e.clientY;
    const startScroll = list.scrollTop;
    const { scrollHeight, clientHeight } = list;
    // 与 update() 一致：thumb 行程按轨道实际高度映射
    const trackHeight = track.clientHeight;
    const height = Math.max(24, (clientHeight / scrollHeight) * trackHeight);
    const maxTop = trackHeight - height;
    const maxScroll = scrollHeight - clientHeight;
    const ratio = maxTop > 0 ? maxScroll / maxTop : 1;
    const onMove = (ev: MouseEvent) => {
      list.scrollTop = startScroll + (ev.clientY - startY) * ratio;
    };
    const onUp = () => {
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
    };
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  }, []);

  // Reset dirty ref when leaving edit mode
  useEffect(() => {
    if (!editing) dirtyRef.current = false;
  }, [editing]);

  const loadTasks = useCallback(async (): Promise<HeartbeatTask[] | null> => {
    setLoading(true);
    try {
      const [taskList, wsList] = await Promise.all([
        heartbeatListTasks(),
        app.ListWorkspaces(),
      ]);
      setTasks(taskList);
      const map: Record<string, string> = {};
      if (wsList) {
        wsList.forEach((ws) => { if (ws.path) map[ws.path] = ws.name; });
      }
      setWorkspaceMap(map);
      return taskList;
    } catch {
      return null;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    setEditing(null);
    setSearchQuery("");
    setStatusFilter("all");
    void loadTasks();
  }, [loadTasks]);

  // Clear editing when the edited task is no longer in the filtered list.
  // Unsaved drafts (created via Add/scoped-Add/startNew) are not yet in
  // `tasks`; keep their editor open until the user saves or cancels.
  // 列表切换任务：未保存修改直接放弃（ChatGPT 式，不弹确认）
  useEffect(() => {
    if (!editing) return;
    if (unsavedDraftIdsRef.current.has(editing.id)) return;
    // 过滤变化导致编辑任务不可见时，直接关闭（丢弃未保存修改）。
    const closeIfNotDirty = () => {
      dirtyRef.current = false;
      return true;
    };
    const match = tasks.find(t => t.id === editing.id);
    if (!match) { if (closeIfNotDirty()) setEditing(null); return; }
    // 状态筛选切换（全部↔已开启↔已暂停）不关闭已打开的详情——右侧详情跟随任务，
    // 与左侧列表筛选解耦（用户明确要求：已激活任务的详情不受筛选影响）。
    if (searchQuery && !match.title.toLowerCase().includes(searchQuery.toLowerCase())) { if (closeIfNotDirty()) setEditing(null); return; }
    // 与列表过滤一致：scopeFilter 过滤掉正在编辑的任务时关闭编辑器。
    if (scopeFilter === "global" && (match.scope === "project" && match.workspaceRoot)) { if (closeIfNotDirty()) setEditing(null); return; }
    if (scopeFilter !== "all" && scopeFilter !== "global" && (match.scope !== "project" || match.workspaceRoot !== scopeFilter)) { if (closeIfNotDirty()) setEditing(null); }
  }, [tasks, editing?.id, statusFilter, searchQuery, scopeFilter, t]);

  const save = useCallback(
    async (next: HeartbeatTask[]): Promise<HeartbeatTask[] | null> => {
      try {
        const persisted = await heartbeatSaveTasks(next);
        setTasks(persisted);
        return persisted;
      } catch {
        // A concurrent external edit wins; reload the authoritative config so
        // the list cannot continue from a stale baseline. The editor keeps its
        // local draft and reports the failure instead of pretending it saved.
        await loadTasks();
        return null;
      }
    },
    [loadTasks],
  );

  const openUnsavedDraft = useCallback((task: HeartbeatTask) => {
    unsavedDraftIdsRef.current.add(task.id);
    setEditing(task);
    setDetailOpen(true);
  }, []);

  const handleAdd = useCallback(async () => {
    let id = `hb-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    try {
      id = await heartbeatGenerateID();
    } catch {
      // 生成失败时使用本地 fallback id，仍可正常打开新建详情
    }
    // 不设 createdAt：isNew=true，详情显示项目字段、删除按钮禁用
    openUnsavedDraft({
      id,
      title: "",
      prompt: "",
      interval: "30m",
      enabled: true,
      approvalMode: "yolo",
      newConversationEachRun: false,
      notifyChannels: false,
    });
  }, [openUnsavedDraft]);

  const handleAddToScope = useCallback(async (scopeKey: string) => {
    let id = `hb-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    try {
      id = await heartbeatGenerateID();
    } catch {      // 生成失败时使用本地 fallback id
    }
    const isProject = scopeKey !== "global";
    openUnsavedDraft({
      id,
      title: "",
      prompt: "",
      interval: "30m",
      enabled: true,
      scope: isProject ? "project" : "global",
      workspaceRoot: isProject ? scopeKey : "",
    });
  }, [openUnsavedDraft]);

  // 建议区：用预设内容打开未保存草稿，由用户确认后再创建。
  // 动态推荐：建议只在"对应标题的任务不存在"时显示——添加后消失，删除后恢复。
  const handleAddSuggestion = useCallback(
    async (sug: Suggestion) => {
      let id = `hb-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      try {
        id = await heartbeatGenerateID();
      } catch {
        // 生成失败时使用本地 fallback id
      }
      // 推荐卡片只打开编辑器，不直接创建并启用任务：预置 prompt 可能涉及
      // 读取对话/扫描本地文件/访问网络等敏感操作，默认禁用 + ask 审批，
      // 由用户在编辑器中确认作用域与权限后再启用。
      const task: HeartbeatTask = {
        id,
        title: sug.title,
        prompt: sug.prompt,
        interval: sug.interval,
        enabled: false,
        approvalMode: "ask",
        newConversationEachRun: false,
        notifyChannels: false,
        scope: "global",
        workspaceRoot: "",
      };
      openUnsavedDraft(task);
    },
    [openUnsavedDraft],
  );

  const handleEdit = useCallback((task: HeartbeatTask) => {
    // 分栏模式下切换任务直接丢弃当前编辑器未保存改动（ChatGPT 式，不确认）。
    dirtyRef.current = false;
    unsavedDraftIdsRef.current.delete(task.id);
    setEditing({ ...task });
    setDetailOpen(true);
  }, []);

  // 列表行点击状态图标切换任务启用/暂停（即时保存）
  const handleToggle = useCallback(async (task: HeartbeatTask) => {
    const idx = tasks.findIndex((t) => t.id === task.id);
    if (idx < 0) return;
    const toggled = { ...task, enabled: !task.enabled };
    const next = [...tasks];
    next[idx] = toggled;
    const saved = await save(next);
    if (saved) {
      const authoritative = saved.find((item) => item.id === task.id) ?? toggled;
      setEditing((prev) => (prev && prev.id === task.id ? mergeEngineRunState({ ...prev, enabled: authoritative.enabled }, authoritative) : prev));
    }
  }, [tasks, save]);

  const handleDelete = useCallback(
    async (id: string) => {
      const next = tasks.filter((t) => t.id !== id);
      return (await save(next)) !== null;
    },
    [tasks, save],
  );

  // 任务行 ⋯ 菜单（仅删除任务）：单个 popover，anchor 动态指向当前点击的按钮
  const [menuTaskId, setMenuTaskId] = useState<string | null>(null);
  const menuAnchorRef = useRef<HTMLButtonElement | null>(null);

  const handleTrigger = useCallback(
    async (id: string) => {
      try {
        await heartbeatTriggerNow(id);
        const fresh = await loadTasks();
        // 详情页正在编辑该任务时，同步最新 run 状态（runHistory/topicId/lastRunAt），
        // 否则详情页 runHistory 区域停留在触发前的旧快照，看不到新记录。
        // 只合并引擎拥有的字段：等待期间用户对 title/prompt/schedule 的草稿
        // 编辑不能被磁盘旧快照覆盖。
        if (fresh) {
          const updated = fresh.find((t) => t.id === id);
          if (updated) {
            setEditing((prev) => (prev && prev.id === id ? mergeEngineRunState(prev, updated) : prev));
          }
        }
      } catch {
        // ignore
      }
    },
    [loadTasks],
  );

  const handleSaveEdit = useCallback(
    async (task: HeartbeatTask) => {
      // 新建任务（无 createdAt，isNew）保存时补 createdAt，使其成为正式任务：
      // isNew=false 后详情头部出现启停/删除按钮，呈现与常规任务完全一致。
      const persisted = task.createdAt ? task : { ...task, createdAt: Date.now() };
      const idx = tasks.findIndex((t) => t.id === task.id);
      const next = [...tasks];
      if (idx >= 0) {
        next[idx] = persisted;
      } else {
        next.push(persisted);
      }
      const saved = await save(next);
      if (!saved) return false;
      // The draft is now persisted; stop treating it as an unsaved draft.
      unsavedDraftIdsRef.current.delete(task.id);
      setEditing({ ...(saved.find((item) => item.id === task.id) ?? persisted) });
      return true;
    },
    [tasks, save],
  );

  const onDividerMouseDown = useCallback((e: React.MouseEvent<HTMLDivElement>) => {
    e.preventDefault();
    const splitEl = e.currentTarget.parentElement;
    if (!splitEl) return;
    const rect = splitEl.getBoundingClientRect();
    const onMove = (ev: MouseEvent) => {
      const pct = ((ev.clientX - rect.left) / rect.width) * 100;
      const clamped = Math.min(70, Math.max(30, pct));
      setListWidthPct(clamped);
      // 拖拽过程中同步缓存（最后一次 onMove 即松手时的值，无需在 onUp 再写）
      try {
        localStorage.setItem("reasonix-heartbeat-list-width", String(clamped));
      } catch {
        // Storage may be unavailable in hardened webviews; in-memory state still works.
      }
    };
    const onUp = () => {
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  }, []);

  const scopeFilterLabel = (filter: string, map: Record<string, string>): string => {
    if (filter === "all") return t("heartbeat.filterAllProjects");
    if (filter === "global") return t("heartbeat.scopeGlobal");
    return map[filter] || filter.split("/").pop() || filter;
  };

  return (
    <div className="heartbeat-page">
      {/* 无详情时：页面顶部一条全宽透明窗口拖拽条 */}
      {!detailOpen && <div className="heartbeat-drag-strip" />}
      <div className="heartbeat-split">
          {/* ── Left column: task list（含列表区头部工具栏） ── */}
          <div className={`heartbeat-split__left${detailOpen ? "" : " heartbeat-split__left--full"}`} style={{ width: detailOpen ? `${listWidthPct}%` : "100%" }}>
            {!detailOpen && (
              <div className="heartbeat-hero">
                <h1 className="heartbeat-hero__title">{t("heartbeat.heroTitle")}</h1>
                <p className="heartbeat-hero__subtitle">{t("heartbeat.heroSubtitle")}</p>
              </div>
            )}
            <div className="heartbeat-toolbar">
              <div className="heartbeat-status-tabs" role="tablist" aria-label={t("heartbeat.filterStatus")}>
                {(["all", "enabled", "disabled"] as const).map((key) => (
                  <button
                    key={key}
                    type="button"
                    role="tab"
                    aria-selected={statusFilter === key}
                    className={`heartbeat-status-tabs__tab${statusFilter === key ? " heartbeat-status-tabs__tab--on" : ""}`}
                    onClick={() => setStatusFilter(key)}
                  >
                    {key === "all" ? t("heartbeat.filterAll") : key === "enabled" ? t("heartbeat.filterEnabled") : t("heartbeat.filterDisabled")}
                  </button>
                ))}
              </div>
              <div className="heartbeat-toolbar__view" style={{ marginLeft: "auto" }}>
                {listView === "flat" && (
                <div className="heartbeat-scope-filter">
                <button
                  ref={scopeFilterRef}
                  className="heartbeat-toolbar__btn heartbeat-toolbar__btn--select"
                  type="button"
                  onClick={() => setScopeFilterOpen((v) => !v)}
                >
                  <span>{scopeFilterLabel(scopeFilter, workspaceMap)}</span>
                  <ChevronsUpDown size={12} />
                </button>
                <AnchoredPopover
                  open={scopeFilterOpen}
                  anchorRef={scopeFilterRef}
                  onClose={() => setScopeFilterOpen(false)}
                  className="heartbeat-filter-menu"
                  placement="bottom"
                >
                  <div className="heartbeat-filter-menu__list" role="listbox">
                    <button
                      className={`heartbeat-filter-menu__option${scopeFilter === "all" ? " heartbeat-filter-menu__option--selected" : ""}`}
                      role="option"
                      aria-selected={scopeFilter === "all"}
                      type="button"
                      onClick={() => { setScopeFilter("all"); setScopeFilterOpen(false); }}
                    >
                      <span>{t("heartbeat.filterAllProjects")}</span>
                      {scopeFilter === "all" && <Check size={12} className="heartbeat-filter-menu__check" />}
                    </button>
                    <button
                      className={`heartbeat-filter-menu__option${scopeFilter === "global" ? " heartbeat-filter-menu__option--selected" : ""}`}
                      role="option"
                      aria-selected={scopeFilter === "global"}
                      type="button"
                      onClick={() => { setScopeFilter("global"); setScopeFilterOpen(false); }}
                    >
                      <span>{t("heartbeat.scopeGlobal")}</span>
                      {scopeFilter === "global" && <Check size={12} className="heartbeat-filter-menu__check" />}
                    </button>
                    {(() => {
                      const seen = new Set<string>();
                      const items: { value: string; label: string }[] = [];
                      for (const task of tasks) {
                        const key = task.scope !== "project" || !task.workspaceRoot ? "global" : task.workspaceRoot;
                        if (seen.has(key)) continue;
                        seen.add(key);
                        if (key !== "global") {
                          items.push({
                            value: key,
                            label: workspaceMap[key] || key.split("/").pop() || key,
                          });
                        }
                      }
                      return items.map((item) => (
                        <button
                          key={item.value}
                          className={`heartbeat-filter-menu__option${scopeFilter === item.value ? " heartbeat-filter-menu__option--selected" : ""}`}
                          role="option"
                          aria-selected={scopeFilter === item.value}
                          type="button"
                          onClick={() => { setScopeFilter(item.value); setScopeFilterOpen(false); }}
                        >
                          <span>{item.label}</span>
                          {scopeFilter === item.value && <Check size={12} className="heartbeat-filter-menu__check" />}
                        </button>
                      ));
                    })()}
                  </div>
                </AnchoredPopover>
                </div>
                )}
                <button
                  className="heartbeat-toolbar__btn heartbeat-toolbar__btn--icon"
                  type="button"
                  onClick={() => setListView(listView === "flat" ? "grouped" : "flat")}
                  title={listView === "flat" ? t("heartbeat.viewGrouped") : t("heartbeat.viewFlat")}
                >
                  {listView === "flat" ? <FolderTree size={14} /> : <List size={14} />}
                </button>
                <button
                  className="heartbeat-toolbar__btn heartbeat-toolbar__btn--primary heartbeat-toolbar__btn--add"
                  type="button"
                  onClick={() => void handleAdd()}
                  title={t("heartbeat.addTask")}
                >
                  <Plus size={14} strokeWidth={2.2} />
                  {t("heartbeat.btnNew")}
                </button>
              </div>
            </div>

            <div className="heartbeat-split__list-wrap">
              <div className="heartbeat-list-search">
                <Search size={13} className="heartbeat-list-search__icon" />
                <input
                  className="heartbeat-list-search__input"
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") setSearchQuery("");
                  }}
                  placeholder={t("heartbeat.searchPlaceholder")}
                />
                {searchQuery && (
                  <button className="heartbeat-list-search__clear" onClick={() => setSearchQuery("")}>
                    <X size={12} />
                  </button>
                )}
              </div>
              <div className="heartbeat-split__list" ref={listRef}>              {(() => {
                const filtered = tasks
                  .filter((task) => {
                    if (statusFilter === "enabled" && !task.enabled) return false;
                    if (statusFilter === "disabled" && task.enabled) return false;
                    if (searchQuery && !task.title.toLowerCase().includes(searchQuery.toLowerCase())) return false;
                    if (scopeFilter === "global" && (task.scope === "project" && task.workspaceRoot)) return false;
                    if (scopeFilter !== "all" && scopeFilter !== "global") {
                      if (task.scope !== "project" || task.workspaceRoot !== scopeFilter) return false;
                    }
                    return true;
                  })
                  .sort(sortTasksByNextRun);

                // Group tasks by scope
                const groups = new Map<string, HeartbeatTask[]>();
                for (const task of filtered) {
                  const key = task.scope === "project" && task.workspaceRoot
                    ? task.workspaceRoot : "global";
                  if (!groups.has(key)) groups.set(key, []);
                  groups.get(key)!.push(task);
                }

                const sortedGroups = Array.from(groups.entries()).sort(([a], [b]) => {
                  if (a === "global") return -1;
                  if (b === "global") return 1;
                  return (workspaceMap[a] || a).localeCompare(workspaceMap[b] || b);
                });

                const toggleProject = (key: string) => {
                  setExpandedProjects((prev) => {
                    if (prev === null) {
                      // All groups are currently expanded (null = all). The
                      // first click must collapse the clicked group while
                      // keeping every other group expanded, so seed the set
                      // with all groups minus the clicked one.
                      const next = new Set<string>();
                      for (const [k] of groups) {
                        if (k !== key) next.add(k);
                      }
                      return next;
                    }
                    const next = new Set(prev);
                    if (next.has(key)) next.delete(key);
                    else next.add(key);
                    return next;
                  });
                };

                const isGroupExpanded = (key: string): boolean => {
                  if (expandedProjects === null) return true;
                  return expandedProjects.has(key);
                };

                return loading ? (
                  <div className="heartbeat-empty">
                    <Heart size={24} className="heartbeat-pulse" />
                    <span>{t("workspace.loading")}</span>
                  </div>
                ) : filtered.length === 0 ? (
                  tasks.length === 0 ? (
                    <div className="heartbeat-empty heartbeat-empty--guided">
                      <Heart size={28} />
                      <span>{t("heartbeat.noTasks")}</span>
                      <button className="heartbeat-btn heartbeat-btn--primary" type="button" onClick={handleAdd}>
                        <Plus size={13} />
                        {t("heartbeat.addTask")}
                      </button>
                    </div>
                  ) : (
                    <div className="heartbeat-empty">
                      <Heart size={24} />
                      <span>{t("heartbeat.noMatchingTasks")}</span>
                    </div>
                  )
                ) : listView === "flat" ? (
                  <div className="worktree-tree heartbeat-flat">
                    {filtered.map((task) => {
                      const isSelected = detailOpen && editing?.id === task.id;
                      const nextRun = taskNextRun(task, t);
                      const scopeLabel = task.scope === "project" && task.workspaceRoot
                        ? (workspaceMap[task.workspaceRoot] || task.workspaceRoot.split("/").pop() || task.workspaceRoot)
                        : t("heartbeat.scopeGlobal");
                      return (
                        <div
                          key={task.id}
                          className={`worktree-node worktree-node--task${task.enabled ? "" : " worktree-node--paused"}${isSelected ? " worktree-node--selected" : ""}`}
                          style={{ paddingLeft: "21px" }}
                          onClick={() => handleEdit(task)}
                        >
                          <div className="worktree-node__main">
                            <span className="worktree-node__marker">
                              <Tooltip
                                label={task.enabled ? t("heartbeat.clickPause") : t("heartbeat.clickStart")}
                                side="top"
                                delay={60}
                              >
                                <button
                                  className={`worktree-node__toggle${task.enabled ? " worktree-node__toggle--on" : ""}`}
                                  type="button"
                                  onClick={(e) => { e.stopPropagation(); void handleToggle(task); }}
                                >
                                  {task.enabled ? (
                                    <>
                                      <Circle className="worktree-node__toggle-circle" size={15} strokeWidth={2.4} />
                                      <CirclePause className="worktree-node__toggle-hover" size={15} strokeWidth={2.4} />
                                    </>
                                  ) : (
                                    <CirclePlaySolid size={15} />
                                  )}
                                </button>
                              </Tooltip>
                            </span>
                            <span className="worktree-node__label">{task.title || t("heartbeat.untitled")}</span>
                            <span className="worktree-node__actions">
                              <button
                                className="worktree-node__action-btn"
                                type="button"
                                onClick={(e) => { e.stopPropagation(); void handleTrigger(task.id); }}
                                title={t("heartbeat.runNow")}
                              >
                                <Play size={14} strokeWidth={1.9} />
                              </button>
                              <button
                                className="worktree-node__action-btn"
                                type="button"
                                disabled={!task.topicId}
                                onClick={(e) => {
                                  e.stopPropagation();
                                  if (task.topicId && onOpenTopic) {
                                    onOpenTopic(task.scope || "global", task.workspaceRoot || "", task.topicId);
                                  }
                                }}
                                title={task.topicId ? t("heartbeat.openTopic") : ""}
                              >
                                <MessageSquare size={14} strokeWidth={1.9} />
                              </button>
                              <button
                                className="worktree-node__action-btn"
                                type="button"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  menuAnchorRef.current = e.currentTarget;
                                  setMenuTaskId(task.id);
                                }}
                                title={t("common.moreActions")}
                              >
                                <MoreHorizontal size={14} strokeWidth={1.9} />
                              </button>
                            </span>
                          </div>
                          <div className="worktree-node__meta">
                            <span className="worktree-node__scope-tag">{scopeLabel}</span>
                            <span className="worktree-node__interval">{formatInterval(task.interval, t)}{nextRun ? ` · ${nextRun}` : ""}</span>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                ) : (
                  <div className="worktree-tree">
                    {sortedGroups.map(([key, groupTasks]) => {
                      const isExpanded = isGroupExpanded(key);
                      const label = key === "global"
                        ? t("heartbeat.scopeGlobal")
                        : workspaceMap[key] || key.split("/").pop() || key;

                      return (
                        <div key={key}>
                          {/* ── Group header (depth 0: 8px indent) ── */}
                          <div
                            className={`worktree-node worktree-node--scope`}
                            style={{ paddingLeft: "8px" }}
                            onClick={() => toggleProject(key)}
                          >
                            <span className="worktree-node__icon">
                              {isExpanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                            </span>
                            <span className="worktree-node__label">{label}</span>
                            <span className="worktree-node__scope-add" onClick={(e) => { e.stopPropagation(); void handleAddToScope(key); }} title={t("heartbeat.addTaskToScope", { name: label })}>
                              <Plus size={12} strokeWidth={2.5} />
                            </span>
                          </div>

                          {/* ── Tasks under group (depth 1: 14 + 16 = 30px indent) ── */}
                          {isExpanded && groupTasks.map((task) => {
                            const isSelected = detailOpen && editing?.id === task.id;
                            const nextRun = taskNextRun(task, t);
                            return (
                              <div
                                key={task.id}
                                className={`worktree-node worktree-node--task${task.enabled ? "" : " worktree-node--paused"}${isSelected ? " worktree-node--selected" : ""}`}
                                style={{ paddingLeft: "21px" }}
                                onClick={() => handleEdit(task)}
                              >
                                <div className="worktree-node__main">
                                  <span className="worktree-node__marker">
                                    <Tooltip
                                      label={task.enabled ? t("heartbeat.clickPause") : t("heartbeat.clickStart")}
                                      side="top"
                                      delay={60}
                                    >
                                      <button
                                        className={`worktree-node__toggle${task.enabled ? " worktree-node__toggle--on" : ""}`}
                                        type="button"
                                        onClick={(e) => { e.stopPropagation(); void handleToggle(task); }}
                                      >
                                        {task.enabled ? (
                                          <>
                                            <Circle className="worktree-node__toggle-circle" size={15} strokeWidth={2.4} />
                                            <CirclePause className="worktree-node__toggle-hover" size={15} strokeWidth={2.4} />
                                          </>
                                        ) : (
                                          <CirclePlaySolid size={15} />
                                        )}
                                      </button>
                                    </Tooltip>
                                  </span>
                                  <span className="worktree-node__label">{task.title || t("heartbeat.untitled")}</span>
                                  <span className="worktree-node__actions">
                                  <button
                                    className="worktree-node__action-btn"
                                    onClick={(e) => { e.stopPropagation(); void handleTrigger(task.id); }}
                                    title={t("heartbeat.runNow")}
                                  >
                                    <Play size={14} strokeWidth={1.9} />
                                  </button>
                                  <button
                                    className="worktree-node__action-btn"
                                    type="button"
                                    disabled={!task.topicId}
                                    onClick={(e) => {
                                      e.stopPropagation();
                                      if (task.topicId && onOpenTopic) {
                                        onOpenTopic(task.scope || "global", task.workspaceRoot || "", task.topicId);
                                      }
                                    }}
                                    title={task.topicId ? t("heartbeat.openTopic") : ""}
                                  >
                                    <MessageSquare size={14} strokeWidth={1.9} />
                                  </button>
                                  <button
                                    className="worktree-node__action-btn"
                                    type="button"
                                    onClick={(e) => {
                                      e.stopPropagation();
                                      menuAnchorRef.current = e.currentTarget;
                                      setMenuTaskId(task.id);
                                    }}
                                    title={t("common.moreActions")}
                                  >
                                    <MoreHorizontal size={14} strokeWidth={1.9} />
                                  </button>
                                </span>
                                </div>
                                <div className="worktree-node__meta">
                                  <span className="worktree-node__interval">{formatInterval(task.interval, t)}{nextRun ? ` · ${nextRun}` : ""}</span>
                                </div>
                              </div>
                            );
                          })}
                        </div>
                      );
                    })}
                  </div>
                );
              })()}
              {statusFilter === "all" && !searchQuery && scopeFilter === "all" && suggestions(t).some((sug) => !tasks.some((task) => task.title === sug.title)) && (
                <div className="heartbeat-suggestions">
                  <div className="heartbeat-suggestions__header">
                    <Lightbulb size={13} />
                    <span>{t("heartbeat.suggestions")}</span>
                    <span className="heartbeat-suggestions__hint">{t("heartbeat.suggestionsHint")}</span>
                  </div>
                  {suggestions(t)
                    .filter((sug) => !tasks.some((task) => task.title === sug.title))
                    .map((sug) => (
                      <button
                        key={sug.id}
                        className="heartbeat-suggestion"
                        type="button"
                        onClick={() => void handleAddSuggestion(sug)}
                      >
                        <div className="heartbeat-suggestion__main">
                          <span className="heartbeat-suggestion__title">{sug.title}</span>
                          <span className="heartbeat-suggestion__freq">{sug.freqLabel}</span>
                        </div>
                        <span className="heartbeat-suggestion__desc">{sug.desc}</span>
                      </button>
                    ))}
                </div>
              )}
              </div>
            </div>
            <div className={`heartbeat-scrollbar${detailOpen ? "" : " heartbeat-scrollbar--edge"}`} aria-hidden="true" ref={scrollbarRef}>
              <div
                ref={thumbRef}
                className="heartbeat-scrollbar__thumb"
                onMouseDown={onScrollThumbMouseDown}
              />
            </div>
          </div>

          {/* 任务行 ⋯ 菜单：仅删除任务 */}
          <AnchoredPopover
            open={menuTaskId !== null}
            anchorRef={menuAnchorRef}
            onClose={() => setMenuTaskId(null)}
            className="heartbeat-task-menu"
            placement="bottom"
          >
            <button
              className="heartbeat-task-menu__item heartbeat-task-menu__item--danger"
              type="button"
              onClick={() => {
                if (menuTaskId) void handleDelete(menuTaskId);
                setMenuTaskId(null);
              }}
            >
              <Trash2 size={13} />
              <span>{t("common.delete")}</span>
            </button>
          </AnchoredPopover>

          {/* ── Vertical divider (draggable, visible only when detail open) ── */}
          {detailOpen && (
            <div className="heartbeat-split__divider" onMouseDown={onDividerMouseDown} />
          )}

          {/* ── Right column: detail / editor (ChatGPT-style, opens on task click) ── */}
          {detailOpen && (
            <div className="heartbeat-split__right">
              {editing ? (
                <TaskEditor key={editing.id} task={editing} onSave={handleSaveEdit} onDelete={async () => {
                  const deleted = await handleDelete(editing.id);
                  if (deleted) {
                    setEditing(null);
                    setDetailOpen(false);
                  }
                  return deleted;
                }} onCloseDetail={() => { setDetailOpen(false); }} onDirtyChange={(d) => { dirtyRef.current = d; }} onOpenTopic={onOpenTopic} onTrigger={handleTrigger} />
              ) : (
                <div className="heartbeat-split__empty">
                  <div className="heartbeat-split__empty-inner">
                    <Activity size={28} />
                    <span>{t("heartbeat.selectTask")}</span>
                    <span className="heartbeat-split__empty-hint">{t("heartbeat.configHint")}</span>
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
  );
}

// ── Cycle Editor ──────────────────────────────────────────────────────────────

const WEEKDAYS = [
  { key: "mon", labelKey: "heartbeat.weekdayMon" },
  { key: "tue", labelKey: "heartbeat.weekdayTue" },
  { key: "wed", labelKey: "heartbeat.weekdayWed" },
  { key: "thu", labelKey: "heartbeat.weekdayThu" },
  { key: "fri", labelKey: "heartbeat.weekdayFri" },
  { key: "sat", labelKey: "heartbeat.weekdaySat" },
  { key: "sun", labelKey: "heartbeat.weekdaySun" },
] as const;

const ALL_WEEKDAYS = WEEKDAYS.map(w => w.key);
const DEFAULT_WEEKLY_DAY = "mon";

// ── 建议区：预填的自动化任务草稿 ──
interface Suggestion {
  id: string;
  title: string;
  desc: string;
  prompt: string;
  interval: string;
  freqLabel: string;
}

function suggestions(t: Translator): Suggestion[] {
  return [
    {
      id: "daily-review",
      title: t("heartbeat.sugDailyReview"),
      desc: t("heartbeat.sugDailyReviewDesc"),
      prompt: t("heartbeat.sugDailyReviewPrompt"),
      interval: "24h|daily@20:00",
      freqLabel: t("heartbeat.sugDailyReviewFreq"),
    },
    {
      id: "product-update",
      title: t("heartbeat.sugProductUpdate"),
      desc: t("heartbeat.sugProductUpdateDesc"),
      prompt: t("heartbeat.sugProductUpdatePrompt"),
      interval: "24h|daily@12:00",
      freqLabel: t("heartbeat.sugProductUpdateFreq"),
    },
    {
      id: "downloads-report",
      title: t("heartbeat.sugDownloads"),
      desc: t("heartbeat.sugDownloadsDesc"),
      prompt: t("heartbeat.sugDownloadsPrompt"),
      interval: "168h|weekly:fri@16:00",
      freqLabel: t("heartbeat.sugDownloadsFreq"),
    },
  ];
}

function defaultHeartbeatCycleDays(cycleType: string): string[] {
  if (cycleType === "daily") return [...ALL_WEEKDAYS];
  if (cycleType === "weekly" || cycleType === "biweekly") return [DEFAULT_WEEKLY_DAY];
  return [];
}

export function heartbeatBuildCycleInterval(cycleType: string, days: string[], time: string): string {
  const base: Record<string, string> = {
    daily: "24h",
    weekly: "168h",
    biweekly: "336h",
    monthly: "720h",
    yearly: "8760h",
  };
  const selectedDays = days.filter(Boolean);
  const isDailyWithSelection = cycleType === "daily" && selectedDays.length > 0 && selectedDays.length < 7;
  const isDailyWithoutSelection = cycleType === "daily" && selectedDays.length === 0;
  const effectiveType = isDailyWithoutSelection || isDailyWithSelection ? "weekly" : cycleType;
  const scheduleDays =
    (effectiveType === "weekly" || effectiveType === "biweekly") && selectedDays.length === 0
      ? defaultHeartbeatCycleDays(effectiveType)
      : selectedDays;

  let suffix = `|${effectiveType}`;
  if (effectiveType === "weekly" || effectiveType === "biweekly") {
    suffix += `:${scheduleDays.join(",")}`;
  } else if (effectiveType === "monthly") {
    suffix += `:${scheduleDays[0] || "1"}`;
  } else if (effectiveType === "yearly") {
    suffix += `:${scheduleDays[0] || "1"}-${scheduleDays[1] || "1"}`;
  }
  suffix += `@${time}`;
  return (base[cycleType] || "24h") + suffix;
}

function CycleEditor({
  draft,
  setDraft,
  cycleType,
}: {
  draft: HeartbeatTask;
  setDraft: (field: keyof HeartbeatTask, value: string | boolean) => void;
  cycleType: "daily" | "weekly" | "biweekly" | "monthly" | "yearly";
}) {
  const t = useT();
  const cycleMatch = (draft.interval || "").match(/^(\d+)[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
  const cycleDays = cycleMatch?.[3] || "";
  const cycleTime = cycleMatch?.[4] || "09:00";
  const [selectedDays, setSelectedDays] = useState<string[]>(
    cycleDays ? cycleDays.split(",") : ["mon","tue","wed","thu","fri","sat","sun"]
  );
  const [monthDay, setMonthDay] = useState(cycleDays || "1");
  const [yearMonth, setYearMonth] = useState(cycleDays.split("-")[0] || "1");
  const [yearDay, setYearDay] = useState(cycleDays.split("-")[1] || "1");
  const [timeVal, setTimeVal] = useState(cycleTime);

  // Build interval string when config changes
  const buildInterval = useCallback((ct: string, days: string[], tm: string) => {
    const base: Record<string, string> = {
      daily: "24h",
      weekly: "168h",
      biweekly: "336h",
      monthly: "720h",
      yearly: "8760h",
    };
    let suffix = `|${ct}`;
    if (ct === "daily" || ct === "weekly" || ct === "biweekly") {
      suffix += `:${days.join(",")}`;
    } else if (ct === "monthly") {
      suffix += `:${days[0] || "1"}`;
    } else if (ct === "yearly") {
      // days[0] = month, days[1] = day — each is a plain number, no dash
      suffix += `:${days[0] || "1"}-${days[1] || "1"}`;
    }
    suffix += `@${tm}`;
    return (base[ct] || "24h") + suffix;
  }, []);

  const onDayToggle = useCallback((day: string) => {
    setSelectedDays((prev) => {
      // Weekly/biweekly schedules must keep at least one weekday selected;
      // an empty weekday rule is rejected by the backend's schedule parser,
      // silently turning the task into a rolling interval.
      const isWeeklyLike = cycleType === "weekly" || cycleType === "biweekly";
      const wouldBeEmpty = prev.includes(day) && prev.length === 1 && isWeeklyLike;
      if (wouldBeEmpty) return prev;
      const next = prev.includes(day) ? prev.filter((d) => d !== day) : [...prev, day];
      setDraft("interval", buildInterval(cycleType, next, timeVal));
      return next;
    });
  }, [buildInterval, cycleType, setDraft, timeVal]);

  const onMonthDayChange = useCallback((d: string) => {
    setMonthDay(d);
    setDraft("interval", buildInterval(cycleType, [d], timeVal));
  }, [buildInterval, cycleType, setDraft, timeVal]);

  const onYearMonthChange = useCallback((m: string) => {
    setYearMonth(m);
    setDraft("interval", buildInterval(cycleType, [m, yearDay], timeVal));
  }, [buildInterval, cycleType, setDraft, timeVal, yearDay]);

  const onYearDayChange = useCallback((d: string) => {
    setYearDay(d);
    setDraft("interval", buildInterval(cycleType, [yearMonth, d], timeVal));
  }, [buildInterval, cycleType, setDraft, timeVal, yearMonth]);

  const onTimeChange = useCallback((tm: string) => {
    setTimeVal(tm);
    const days = cycleType === "daily" || cycleType === "weekly" || cycleType === "biweekly" ? selectedDays
      : cycleType === "monthly" ? [monthDay]
      : cycleType === "yearly" ? [yearMonth, yearDay]
      : [];
    setDraft("interval", buildInterval(cycleType, days, tm));
  }, [buildInterval, cycleType, selectedDays, monthDay, yearMonth, yearDay, setDraft]);

  const MONTHS = Array.from({ length: 12 }, (_, i) => ({
    value: String(i + 1),
    label: t("heartbeat.monthOption", { n: i + 1 }),
  }));
  const DAYS = Array.from({ length: 31 }, (_, i) => ({
    value: String(i + 1),
    label: t("heartbeat.dayOption", { n: i + 1 }),
  }));

  return (
    <div className="heartbeat-editor__cycle-wrap">
      <div className="heartbeat-editor__cycle-row">
        {cycleType === "monthly" && (
          <select
            className="heartbeat-editor__freq-select"
            value={monthDay}
            onChange={(e) => onMonthDayChange(e.target.value)}
          >
            {DAYS.map((d) => (
              <option key={d.value} value={d.value}>{d.label}</option>
            ))}
          </select>
        )}

        {cycleType === "yearly" && (
          <>
            <select
              className="heartbeat-editor__freq-select"
              value={yearMonth}
              onChange={(e) => onYearMonthChange(e.target.value)}
            >
              {MONTHS.map((m) => (
                <option key={m.value} value={m.value}>{m.label}</option>
              ))}
            </select>
            <select
              className="heartbeat-editor__freq-select"
              value={yearDay}
              onChange={(e) => onYearDayChange(e.target.value)}
            >
              {DAYS.map((d) => (
                <option key={d.value} value={d.value}>{d.label}</option>
              ))}
            </select>
          </>
        )}

        <input
          className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time"
          type="time"
          value={timeVal}
          onChange={(e) => onTimeChange(e.target.value)}
        />

        {(cycleType === "weekly" || cycleType === "biweekly") && (
          <div className="set-seg">
            {WEEKDAYS.map((wd) => (
              <button
                key={wd.key}
                type="button"
                className={`set-seg__btn${selectedDays.includes(wd.key) ? " set-seg__btn--on" : ""}`}
                onClick={() => onDayToggle(wd.key)}
                aria-pressed={selectedDays.includes(wd.key)}
              >
                {t(wd.labelKey)}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Editor ─────────────────────────────────────────────────────────────────────

function normalizeMode(mode: "ask" | "auto" | "yolo" | undefined): "ask" | "auto" | "yolo" {
  if (mode === "ask" || mode === "auto" || mode === "yolo") return mode;
  return "yolo"; // default
}

export function TaskEditor({
  task,
  onSave,
  onDelete,
  onCloseDetail,
  onDirtyChange,
  onOpenTopic,
  onTrigger,
}: {
  task: HeartbeatTask;
  onSave: (t: HeartbeatTask) => Promise<boolean>;
  onDelete: () => Promise<boolean>;
  onCloseDetail: () => void;
  onDirtyChange?: (dirty: boolean) => void;
  onOpenTopic?: (scope: string, workspaceRoot: string, topicId: string) => void;
  onTrigger?: (id: string) => void;
}) {
  const t = useT();
  const titleRef = useRef<HTMLInputElement>(null);
  const [workspaces, setWorkspaces] = useState<WorkspaceView[]>([]);
  const [projectOpen, setProjectOpen] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState(false);
  const [frequencyError, setFrequencyError] = useState(false);
  const projectRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    app.ListWorkspaces().then((list) => setWorkspaces(list ?? [])).catch(() => {});
  }, []);

  useEffect(() => {
    if (!projectOpen) return;
    const close = (e: MouseEvent) => {
      if (projectRef.current && !projectRef.current.contains(e.target as Node)) {
        setProjectOpen(false);
      }
    };
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [projectOpen]);

  const [draft, setDraft] = useState(task);
  const initialTaskRef = useRef(task);
  // 保存后父组件 setEditing({...task}) 传入新引用，同步基线使 isDirty
  // 复位（保存按钮与 dirtyRef 不再保持脏状态）。
  useEffect(() => {
    initialTaskRef.current = task;
  }, [task]);
  // Sync fields owned outside this editor without replacing user-owned draft
  // input. In particular, TriggerNow may advance topicId/lastRunAt/runHistory
  // while the user is editing title, prompt, or schedule.
  useEffect(() => {
    setDraft((current) => mergeEngineRunState({ ...current, enabled: task.enabled }, task));
  }, [task.enabled, task.lastRunAt, task.runHistory, task.topicId]);
  const isNew = !task.createdAt;
  const isDirty = draft.title !== initialTaskRef.current.title
    || draft.prompt !== initialTaskRef.current.prompt
    || draft.interval !== initialTaskRef.current.interval
    || draft.enabled !== initialTaskRef.current.enabled
    || draft.approvalMode !== initialTaskRef.current.approvalMode
    || draft.newConversationEachRun !== initialTaskRef.current.newConversationEachRun
    || draft.notifyChannels !== initialTaskRef.current.notifyChannels
    || draft.scope !== initialTaskRef.current.scope
    || draft.workspaceRoot !== initialTaskRef.current.workspaceRoot
    || draft.timeWindowStart !== initialTaskRef.current.timeWindowStart
    || draft.timeWindowEnd !== initialTaskRef.current.timeWindowEnd;

  useEffect(() => {
    onDirtyChange?.(isDirty);
  }, [isDirty, onDirtyChange]);

  const promptRef = useRef<HTMLTextAreaElement>(null);

  // Auto-grow prompt textarea: shrink-to-fit then cap at 180px
  const autoGrowPrompt = useCallback(() => {
    const el = promptRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 180) + "px";
  }, []);

  useLayoutEffect(() => {
    autoGrowPrompt();
  }, [draft.prompt, autoGrowPrompt]);

  // 手动保存（ChatGPT 式）：修改后底部出现取消/保存，无修改时不显示。
  // enabled 开关走头部即时保存并同步基线，不进入 isDirty。
  const handleCancel = useCallback(() => {
    setDraft(initialTaskRef.current);
  }, []);

  const handleSave = useCallback(async () => {
    if (!draft.title.trim() || !draft.prompt.trim()) return;
    setSaving(true);
    const saved = await onSave(draft);
    setSaving(false);
    setSaveError(!saved);
  }, [draft, onSave]);
  const set = useCallback((field: keyof HeartbeatTask, value: string | boolean) => {
    setDraft((prev) => ({ ...prev, [field]: value }));
  }, []);

  // 启用/暂停切换（状态文字入口 + 右侧按钮共用）：
  // 只持久化 enabled 变更，基于最近保存基线（initialTaskRef）翻转，
  // 不携带 draft 中尚未保存的 title/prompt/schedule 编辑；同时保留草稿，
  // 等待中的用户输入不被基线快照覆盖。
  const toggleEnabled = useCallback(async () => {
    const saved = initialTaskRef.current;
    const updated = { ...saved, enabled: !saved.enabled };
    setSaving(true);
    const persisted = await onSave(updated);
    setSaving(false);
    setSaveError(!persisted);
    if (persisted) {
      // 只同步 enabled（磁盘已持久化），title/prompt/interval 等草稿编辑保留。
      setDraft((prev) => ({ ...prev, enabled: updated.enabled }));
      initialTaskRef.current = { ...saved, enabled: updated.enabled };
    }
  }, [onSave]);

  // Detect frequency type from interval value
  const [freqType, setFreqType] = useState<HeartbeatFrequencyType>(
    (() => {
      const iv = task.interval || "";
      if (isCronExpr(iv)) return "cron";
      const m = iv.match(/^(\d+)[smh]\|(daily|weekly|biweekly|monthly|yearly)/);
      if (m) return m[2] as "daily" | "weekly" | "biweekly" | "monthly" | "yearly";
      return "interval";
    })()
  );

  // 切换频率类型时重建 interval（摊开的 7 个选项）
  const onFreqSelect = useCallback((ft: HeartbeatFrequencyType) => {
    const converted = changeHeartbeatFrequency(draft, ft);
    if (converted === null) {
      setFrequencyError(true);
      return;
    }
    setFrequencyError(false);
    setDraft(converted);
    setFreqType(ft);
  }, [draft]);

  const selectedWorkspace = draft.scope === "project" && draft.workspaceRoot
    ? workspaces.find((w) => w.path === draft.workspaceRoot)
    : null;

  return (
    <div className="heartbeat-editor">
      {/* Header: 状态文字（点击切换，CodeX 式）+ 操作菜单 + 关闭 */}
      <header className="heartbeat-editor__header">
        {isNew ? (
          <span className="heartbeat-editor__status heartbeat-editor__status--new">{t("heartbeat.newTask")}</span>
        ) : (
          <button
            className={`heartbeat-editor__status${draft.enabled ? " heartbeat-editor__status--on" : ""}`}
            type="button"
            title={draft.enabled ? t("heartbeat.statusDisabled") : t("heartbeat.statusEnabled")}
            disabled={saving}
            onClick={() => void toggleEnabled()}
          >
            {draft.enabled ? t("heartbeat.statusEnabled") : t("heartbeat.statusDisabled")}
          </button>
        )}
        <span className="heartbeat-editor__header-spacer" />
        {!isNew && (
          <Tooltip
            label={t("heartbeat.runNow")}
            side="top"
            delay={60}
          >
            <button
              className="heartbeat-editor__header-action"
              type="button"
              onClick={() => { if (onTrigger) onTrigger(task.id); }}
            >
              <Play size={14} strokeWidth={1.9} />
              {t("heartbeat.runNow")}
            </button>
          </Tooltip>
        )}
        {!isNew && (
          <Tooltip
            label={draft.enabled ? t("heartbeat.clickPause") : t("heartbeat.clickStart")}
            side="top"
            delay={60}
          >
            <button
              className={`heartbeat-editor__header-action${draft.enabled ? " heartbeat-editor__header-action--on" : ""}`}
              type="button"
              disabled={saving}
              onClick={() => void toggleEnabled()}
            >
              {draft.enabled ? (
                <CirclePause size={14} strokeWidth={2.4} />
              ) : (
                <CirclePlaySolid size={14} />
              )}
              {draft.enabled ? t("heartbeat.btnPause") : t("heartbeat.btnStart")}
            </button>
          </Tooltip>
        )}
        {!isNew && (
          <Tooltip
            label={confirmingDelete ? t("heartbeat.confirmDelete") : t("heartbeat.delete")}
            side="top"
            delay={60}
          >
            <button
              className={`heartbeat-editor__header-action heartbeat-editor__header-action--danger${confirmingDelete ? " heartbeat-editor__header-action--confirm" : ""}`}
              type="button"
              disabled={saving}
              onClick={() => {
                if (confirmingDelete) {
                  void onDelete().then((deleted) => setSaveError(!deleted));
                } else {
                  setConfirmingDelete(true);
                  window.setTimeout(() => setConfirmingDelete(false), 3000);
                }
              }}
            >
              <Trash2 size={14} />
              {confirmingDelete ? t("heartbeat.confirmDelete") : t("heartbeat.delete")}
            </button>
          </Tooltip>
        )}
        <button className="heartbeat-editor__close" type="button" onClick={onCloseDetail} title={t("common.close")}>
          <X size={14} />
        </button>
      </header>

      {/* Fields: 表单滚动区 */}
      <div className="heartbeat-editor__fields">
      {/* Title: 隐形输入框——无边框大标题样式，点击仍可直接编辑 */}
      <input
        ref={titleRef}
        className="heartbeat-editor__title"
        value={draft.title}
        onChange={(e) => set("title", e.target.value)}
        placeholder={t("heartbeat.titlePlaceholder")}
        aria-label={t("heartbeat.fieldTitle")}
      />

      {/* Scope：仅新建任务时可选项目，保存后锁定（已创建任务不显示项目字段） */}
      {isNew && (
        <div className="heartbeat-editor__field">
          <label>{t("heartbeat.scopeProject")}</label>
          <div className="heartbeat-scope-wrap" ref={projectRef}>
          <button
            className="heartbeat-scope-select"
            onClick={() => setProjectOpen((v) => !v)}
          >
            {selectedWorkspace ? selectedWorkspace.name : t("heartbeat.scopeGlobal")}
            <ChevronsUpDown size={12} />
          </button>
          {projectOpen && (
            <div className="heartbeat-project-menu">
              {workspaces.length === 0 ? (
                <div className="heartbeat-project-menu__empty">{t("heartbeat.noProjects")}</div>
              ) : (
                <>
                  <button
                    className={`heartbeat-project-menu__item${!draft.scope || draft.scope === "global" || !draft.workspaceRoot ? " heartbeat-project-menu__item--active" : ""}`}
                    onClick={() => {
                      setDraft((prev) => ({ ...prev, scope: "global", workspaceRoot: "" }));
                      setProjectOpen(false);
                    }}
                  >
                    {t("heartbeat.scopeGlobal")}
                    {(!draft.scope || draft.scope === "global" || !draft.workspaceRoot) && <Check size={12} className="heartbeat-filter-menu__check" />}
                  </button>
                  {workspaces.map((ws) => (
                    <button
                      key={ws.path}
                      className={`heartbeat-project-menu__item${draft.workspaceRoot === ws.path ? " heartbeat-project-menu__item--active" : ""}`}
                      onClick={() => {
                        setDraft((prev) => ({ ...prev, scope: "project", workspaceRoot: ws.path }));
                        setProjectOpen(false);
                      }}
                    >
                      {ws.name}
                      {ws.current && <span className="heartbeat-project-menu__current">{t("heartbeat.currentWorkspace")}</span>}
                      {draft.workspaceRoot === ws.path && <Check size={12} className="heartbeat-filter-menu__check" />}
                    </button>
                  ))}
                </>
              )}
            </div>
          )}
        </div>
      </div>
      )}

      {/* Prompt（无字段标题） */}
      <div className="heartbeat-editor__field">
        <textarea
          className="heartbeat-editor__textarea"
          value={draft.prompt}
          onChange={(e) => set("prompt", e.target.value)}
          placeholder={t("heartbeat.promptPlaceholder")}
          rows={5}
        />
      </div>

      {/* Approval Mode（竖排） */}
      <div className="heartbeat-editor__field">
          <label>{t("heartbeat.fieldApprovalMode")}</label>
          <div className="set-seg" style={{ alignSelf: "flex-start" }}>
            <button
              className={`set-seg__btn${normalizeMode(draft.approvalMode) === "ask" ? " set-seg__btn--on" : ""}`}
              onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "ask" }))}
              title={t("heartbeat.approvalModeAskTooltip")}
            >
              {t("heartbeat.approvalModeAsk")}
            </button>
            <button
              className={`set-seg__btn${normalizeMode(draft.approvalMode) === "auto" ? " set-seg__btn--on" : ""}`}
              onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "auto" }))}
              title={t("heartbeat.approvalModeAutoTooltip")}
            >
              {t("heartbeat.approvalModeAuto")}
            </button>
            <button
              className={`set-seg__btn${normalizeMode(draft.approvalMode) === "yolo" ? " set-seg__btn--on" : ""}`}
              onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "yolo" }))}
              title={t("heartbeat.approvalModeYoloTooltip")}
            >
              {t("heartbeat.approvalModeYolo")}
            </button>
          </div>
          <span className="heartbeat-editor__mode-hint">
            {normalizeMode(draft.approvalMode) === "yolo" ? t("heartbeat.approvalModeYoloHint") :
             normalizeMode(draft.approvalMode) === "auto" ? t("heartbeat.approvalModeAutoHint") :
             t("heartbeat.approvalModeAskHint")}
          </span>
        </div>

        {/* Push to bot channels */}
        <div className="heartbeat-editor__field">
          <label>{t("heartbeat.notifyChannels")} <span className="heartbeat-editor__optional">{t("heartbeat.optional")}</span></label>
          <div className="set-seg" style={{ alignSelf: "flex-start" }}>
            <button
              className={`set-seg__btn${draft.notifyChannels === true ? " set-seg__btn--on" : ""}`}
              onClick={() => setDraft((prev) => ({ ...prev, notifyChannels: true }))}
            >
              {t("heartbeat.notifyChannelsOn")}
            </button>
            <button
              className={`set-seg__btn${draft.notifyChannels !== true ? " set-seg__btn--on" : ""}`}
              onClick={() => setDraft((prev) => ({ ...prev, notifyChannels: false }))}
            >
              {t("heartbeat.notifyChannelsOff")}
            </button>
          </div>
          <span className="heartbeat-editor__mode-hint">
            {draft.notifyChannels === true
              ? t("heartbeat.notifyChannelsOnHint")
              : t("heartbeat.notifyChannelsOffHint")}
          </span>
        </div>

      {/* New conversation per run */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldNewConversation")}</label>
        <div className="set-seg" style={{ alignSelf: "flex-start" }}>
          <button
            className={`set-seg__btn${!draft.newConversationEachRun ? " set-seg__btn--on" : ""}`}
            onClick={() => setDraft((prev) => ({ ...prev, newConversationEachRun: false }))}
          >
            {t("heartbeat.newConversationEachRunOff")}
          </button>
          <button
            className={`set-seg__btn${draft.newConversationEachRun ? " set-seg__btn--on" : ""}`}
            onClick={() => setDraft((prev) => ({ ...prev, newConversationEachRun: true }))}
          >
            {t("heartbeat.newConversationEachRunOn")}
          </button>
        </div>
      </div>

      {/* Frequency */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldInterval")}</label>
        <div className="set-seg" style={{ alignSelf: "flex-start", flexWrap: "wrap" }}>
          {([
            ["interval", t("heartbeat.freqInterval")],
            ["daily", t("heartbeat.cycleDaily")],
            ["weekly", t("heartbeat.cycleWeekly")],
            ["biweekly", t("heartbeat.cycleBiweekly")],
            ["monthly", t("heartbeat.cycleMonthly")],
            ["yearly", t("heartbeat.cycleYearly")],
            ["cron", t("heartbeat.freqCron")],
          ] as const).map(([v, label]) => (
            <button
              key={v}
              type="button"
              className={`set-seg__btn${freqType === v ? " set-seg__btn--on" : ""}`}
              onClick={() => onFreqSelect(v)}
            >
              {label}
            </button>
          ))}
        </div>
        {frequencyError && (
          <span className="heartbeat-editor__inline-error" role="status">{t("heartbeat.frequencyConversionFailed")}</span>
        )}

        {freqType === "cron" ? (
          <div className="heartbeat-editor__freq-interval">
            <input
              className="heartbeat-editor__freq-input heartbeat-editor__freq-input--cron"
              value={draft.interval}
              onChange={(e) => setDraft((prev) => ({ ...prev, interval: e.target.value }))}
              placeholder={t("heartbeat.cronPlaceholder")}
            />
            <span className="heartbeat-editor__cron-hint">
              {describeCron(draft.interval, t)}
              {nextCronRunAt(draft.interval) ? ` ${t("heartbeat.cronNextRun")} ${formatCronNext(nextCronRunAt(draft.interval))}` : ""}
            </span>
          </div>
        ) : freqType === "interval" ? (
          <div className="heartbeat-editor__freq-interval">
            <span className="heartbeat-editor__freq-label">{t("heartbeat.freqEvery")}</span>
            <input
              className="heartbeat-editor__freq-input"
              value={(() => {
                const m = (draft.interval || "").match(/^(\d+)/);
                return m ? m[1] : "1";
              })()}
              onChange={(e) => {
                const num = e.target.value.replace(/\D/g, "");
                const mUnit = (draft.interval || "").match(/^(\d+)([smh])/);
                const unit = mUnit ? mUnit[2] : "h";
                setDraft((prev) => ({ ...prev, interval: num ? num + unit : "1" + unit }));
              }}
              placeholder="1"
            />
            <div className="set-seg">
              <button
                className={`set-seg__btn${(() => {
                  const m = (draft.interval || "").match(/^(\d+)([smh])/);
                  return (m ? m[2] : "h") === "m" ? " set-seg__btn--on" : "";
                })()}`}
                onClick={() => {
                  const num = (draft.interval || "").match(/^(\d+)/)?.[1] || "1";
                  setDraft((prev) => ({ ...prev, interval: num + "m" }));
                }}
              >
                {t("heartbeat.unitMin")}
              </button>
              <button
                className={`set-seg__btn${(() => {
                  const m = (draft.interval || "").match(/^(\d+)([smh])/);
                  return (m ? m[2] : "h") === "h" ? " set-seg__btn--on" : "";
                })()}`}
                onClick={() => {
                  const num = (draft.interval || "").match(/^(\d+)/)?.[1] || "1";
                  setDraft((prev) => ({ ...prev, interval: num + "h" }));
                }}
              >
                {t("heartbeat.unitHour")}
              </button>
            </div>
            {draft.timeWindowStart || draft.timeWindowEnd ? (
              <div className="heartbeat-editor__tw-inputs" style={{ marginLeft: "8px" }}>
                <input
                  className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time"
                  type="time"
                  value={draft.timeWindowStart || ""}
                  onChange={(e) => setDraft((prev) => ({ ...prev, timeWindowStart: e.target.value || undefined }))}
                  style={{ width: "90px" }}
                />
                <span className="heartbeat-editor__freq-label heartbeat-editor__tw-sep">—</span>
                <input
                  className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time"
                  type="time"
                  value={draft.timeWindowEnd || ""}
                  onChange={(e) => setDraft((prev) => ({ ...prev, timeWindowEnd: e.target.value || undefined }))}
                  style={{ width: "90px" }}
                />
                <button
                  className="heartbeat-editor__tw-remove"
                  onClick={() => setDraft((prev) => ({ ...prev, timeWindowStart: undefined, timeWindowEnd: undefined }))}
                  title={t("heartbeat.removeTimeWindow")}
                >
                  <X size={12} />
                </button>
              </div>
            ) : (
              <span className="heartbeat-editor__tw-add" style={{ marginLeft: "8px" }}
                onClick={() => setDraft((prev) => ({ ...prev, timeWindowStart: "09:00", timeWindowEnd: "17:00" }))}
              >
                + {t("heartbeat.timeWindow")}
              </span>
            )}
          </div>
        ) : (
          <CycleEditor
            key={freqType}
            draft={draft}
            setDraft={set}
            cycleType={freqType as "daily" | "weekly" | "biweekly" | "monthly" | "yearly"}
          />
        )}
      </div>

      {/* 运行历史记录：每次成功执行的记录，点击可打开对应对话
          历史为空但有最近会话（task.topicId）时，用最近会话合成一条——旧任务
          在 runHistory 字段引入前执行过，topicId 仍指向最近对话 */}
      <div className="heartbeat-run-history">
        <div className="heartbeat-run-history__header">
          <span>{t("heartbeat.runHistory")}</span>
        </div>
        {(() => {
          const history = (task.runHistory || []).length > 0
            ? [...task.runHistory!].reverse()
            : task.topicId
              ? [{ at: task.lastRunAt || task.createdAt || Date.now(), topicId: task.topicId }]
              : [];
          if (history.length === 0) {
            return <div className="heartbeat-run-history__empty">{t("heartbeat.runHistoryEmpty")}</div>;
          }
          return (
            <div className="heartbeat-run-history__list">
              {history.map((run, i) => (
                <button
                  key={`${run.at}-${i}`}
                  className="heartbeat-run-history__item"
                  type="button"
                  disabled={!run.topicId}
                  onClick={() => {
                    if (run.topicId && onOpenTopic) {
                      onOpenTopic(task.scope || "global", task.workspaceRoot || "", run.topicId);
                    }
                  }}
                  title={run.topicId ? t("heartbeat.openTopic") : ""}
                >
                  <span className="heartbeat-run-history__title">{task.title || t("heartbeat.untitled")}</span>
                  <span className="heartbeat-run-history__scope">
                    {task.scope === "project" && task.workspaceRoot
                      ? (workspaces.find((w) => w.path === task.workspaceRoot)?.name
                        || task.workspaceRoot.split("/").pop() || task.workspaceRoot)
                      : t("heartbeat.scopeGlobal")}
                  </span>
                  <span className="heartbeat-run-history__rel">{formatRelativeTime(run.at, Date.now(), t)}</span>
                  {!run.topicId && (
                    <span className="heartbeat-run-history__notopic">{t("heartbeat.runHistoryNoTopic")}</span>
                  )}
                </button>
              ))}
            </div>
          );
        })()}
      </div>
      </div>

      {/* 保存/取消：仅在有未保存修改时显示（ChatGPT 式），固定在面板底部 */}
      {saveError && (
        <div className="heartbeat-editor__save-notice">
          <span className="heartbeat-editor__save-error" role="alert">{t("heartbeat.saveFailed")}</span>
        </div>
      )}
      {(isNew || isDirty) && (
        <div className="heartbeat-editor__actions">
          <button
            className="heartbeat-editor__action-btn"
            type="button"
            onClick={handleCancel}
          >
            {t("common.cancel")}
          </button>
          <button
            className="heartbeat-editor__action-btn heartbeat-editor__action-btn--primary"
            type="button"
            disabled={saving || !draft.title.trim() || !draft.prompt.trim()}
            onClick={() => void handleSave()}
          >
            {t("common.save")}
          </button>
        </div>
      )}
    </div>
  );
}
