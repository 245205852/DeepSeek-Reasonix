export type InlineMathDecision = "math" | "currency" | "literal";

export interface InlineMathContext {
  before?: string;
  after?: string;
}

const PURE_NUMBER = /^\d+(?:,\d{3})*(?:\.\d+)?$/;

const CURRENCY_BEFORE =
  /(?:\b(?:costs?|prices?|priced|pay|paid|worth|fees?|budget|salary|total|amount|usd|dollars?)\b|(?:价格|售价|花费|成本|预算|金额|合计|美元|美金))\s*(?:(?:is|was|are|were|of|at)\s+|[:=]\s*)?$/i;

const CURRENCY_AFTER =
  /^\s*(?:usd\b|dollars?\b|each\b|per\b|\/\s*(?:day|week|month|year)\b|美元|美金|每(?:个|天|周|月|年))/i;

/**
 * Decide how an `inlineMath` node produced by remark-math should render.
 *
 * Dollar delimiters are treated as an explicit math signal by default. The
 * only numeric exception is a high-confidence currency context, where a
 * paired input such as `costs $5$ today` is repaired to the prose currency
 * token `$5`. Non-math word-like content is restored verbatim as text.
 */
export function classifyInlineMath(
  math: string,
  context: InlineMathContext = {},
): InlineMathDecision {
  if (!math || math !== math.trim() || math.includes("\n")) return "literal";
  if (math.includes("://") || math.includes("](")) return "literal";

  if (PURE_NUMBER.test(math)) {
    if (CURRENCY_BEFORE.test(context.before ?? "") || CURRENCY_AFTER.test(context.after ?? "")) {
      return "currency";
    }
    return "math";
  }

  // A percentage enclosed in explicit delimiters is mathematical notation,
  // not a currency amount. latexNormalizeForKatex escapes its `%` for KaTeX.
  if (/^\d+(?:,\d{3})*(?:\.\d+)?%$/.test(math)) return "math";

  // Number followed by variable: implicit multiplication (2.5x, 3y^2).
  if (/^\d+(?:\.\d+)?[A-Za-z](?:[A-Za-z0-9^_{}]*)?$/.test(math)) return "math";
  // Number with LaTeX escape: 10\%, 5\cdot3.
  if (/^\d+(?:\.\d+)?\\(?:%|[A-Za-z]+)(?:\{[^{}]*\})?(?:[A-Za-z0-9\\{}^_+\-*/=<>.()]*)$/.test(math)) return "math";

  // Unary plus/minus: +2, -x, +\alpha, - 3.14.
  if (/^[+\-]\s*(?:\d+(?:\.\d+)?|[A-Za-z\\])/.test(math)) return "math";

  // LaTeX command (\alpha, \frac{x}{y}, \tfrac12, ...).
  if (/\\[A-Za-z]+/.test(math)) return "math";
  if (/[\^_{}|]/.test(math)) return "math";
  if (/^[+\-=<>±∓]$/.test(math)) return "math";
  if (/\b(?:alpha|beta|gamma|sum|int|prod|lim|infty|sqrt|frac|sin|cos|tan|log|ln|max|min|partial|nabla|left|right)\b/.test(math)) return "math";
  if (/^[A-Za-z]{1,6}\s*\([^)]{1,80}\)$/.test(math)) return "math";
  if (/^[A-Za-z]'{1,}\s*\([^)]{1,80}\)$/.test(math)) return "math";
  if (/^(?:\(\d+\))+$/.test(math)) return "math";
  if (/[A-Za-z0-9)\]}]\s*[+\-*/=<>]\s*[+\-]?\s*[A-Za-z0-9([{\\]/.test(math)) return "math";

  // One-sided relation/equality against an implicit operand. The complete
  // anchors are important: `=1 dollar` must not be accepted by a prefix match.
  const operand = String.raw`(?:[+\-]?\s*(?:\d+(?:\.\d+)?|[A-Za-z]|\\[A-Za-z]+))`;
  const relation = String.raw`(?:<=?|>=?|=|≠|≤|≥)`;
  if (new RegExp(`^(?:${relation}\\s*${operand}|${operand}\\s*${relation})$`).test(math)) return "math";

  if (/^\(?(?:[A-Za-z0-9]|\\[A-Za-z]+)(?:\s*,\s*(?:[A-Za-z0-9]|\\[A-Za-z]+)){1,10}\)?$/.test(math)) return "math";
  if (/^\[[A-Za-z0-9^_+\-,.\\\s{}]+\]$/.test(math)) return "math";

  if (/[A-Za-z]\s+[A-Za-z]/.test(math)) return "literal";
  if (/^[A-Z][A-Z0-9_]{1,}$/.test(math)) return "literal";
  if (/^v\d+(?:\.\d+)*$/i.test(math)) return "literal";
  if (/^[A-Za-z]{2,}$/.test(math)) return "literal";

  if (/^(?:[A-Za-z]|\\[A-Za-z]+)'{1,}$/.test(math)) return "math";
  return /^[A-Za-z]$/.test(math) ? "math" : "literal";
}

export function isLikelyInlineMath(math: string): boolean {
  return classifyInlineMath(math) === "math";
}
