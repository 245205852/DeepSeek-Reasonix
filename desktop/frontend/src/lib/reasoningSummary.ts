const DEFAULT_SUMMARY_CHARS = 180;

export type ReasoningSummaryOptions = {
  /** While streaming, the tail line is the live signal; once done, the head line reads best. */
  streaming: boolean;
  maxChars?: number;
};

// Scans forward for the first non-blank line without splitting the whole
// string — long streaming reasoning must not allocate a full line array per
// token.
function firstNonBlankLine(text: string): string {
  let start = 0;
  while (start < text.length) {
    let end = start;
    while (end < text.length && text[end] !== "\n" && text[end] !== "\r") end += 1;
    if (text.slice(start, end).trim()) return text.slice(start, end);
    start = text[end] === "\r" && text[end + 1] === "\n" ? end + 2 : end + 1;
  }
  return "";
}

// Scans backward for the last non-blank line.
function lastNonBlankLine(text: string): string {
  let end = text.length;
  while (end > 0) {
    let start = end;
    while (start > 0 && text[start - 1] !== "\n" && text[start - 1] !== "\r") start -= 1;
    if (text.slice(start, end).trim()) return text.slice(start, end);
    if (start === 0) return "";
    end = start - 1;
  }
  return "";
}

function normalizedMaxChars(maxChars: number): number {
  if (!Number.isFinite(maxChars)) return DEFAULT_SUMMARY_CHARS;
  return Math.max(0, Math.floor(maxChars));
}

// Code-point-safe truncation so a surrogate pair is never split. Streaming
// keeps the tail because that is where newly arrived text appears.
function truncateSummary(text: string, maxChars: number, fromEnd: boolean): string {
  const limit = normalizedMaxChars(maxChars);
  if (limit === 0) return "";
  const chars = Array.from(text);
  if (chars.length <= limit) return text;
  if (limit === 1) return "…";
  return fromEnd
    ? `…${chars.slice(-(limit - 1)).join("")}`
    : `${chars.slice(0, limit - 1).join("")}…`;
}

/** Builds a single-line plain-text preview without invoking another model. */
export function reasoningSummaryText(reasoning: string, { streaming, maxChars = DEFAULT_SUMMARY_CHARS }: ReasoningSummaryOptions): string {
  if (!reasoning.trim()) return "";
  const line = streaming ? lastNonBlankLine(reasoning) : firstNonBlankLine(reasoning);
  return truncateSummary(line.trim().replace(/\s+/g, " "), maxChars, streaming);
}
