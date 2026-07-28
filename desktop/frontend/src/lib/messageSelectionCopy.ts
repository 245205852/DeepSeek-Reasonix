const MESSAGE_SELECTION_SELECTOR = ".msg__body, .reasoning__body";

export interface MessageSelectionCopyState {
  text: string;
  isCollapsed: boolean;
  targetIsEditable: boolean;
  intersectsMessage: boolean;
  canWriteClipboard: boolean;
}

export function messageSelectionCopyText(state: MessageSelectionCopyState): string | null {
  if (state.isCollapsed) return null;
  if (state.targetIsEditable) return null;
  if (!state.intersectsMessage) return null;
  if (!state.canWriteClipboard) return null;
  if (state.text.trim() === "") return null;
  return state.text;
}

// Gates the transcript right-click menu the same way the copy interceptor
// gates ⌘C — a non-collapsed, non-editable selection touching a message or
// reasoning body — plus one menu-only rule: the right-click must land inside
// a message body that the selection itself touches. A selection outlives
// clicks elsewhere: surfaces like the project tree and tab bar own their
// right-click menus, and offering Copy on message B while message A holds
// the selection would copy text from somewhere other than the click. A
// selection spanning several messages accepts a right-click on any of them.
// Returns the text the menu would copy, or null when the menu should not
// open. canWriteClipboard is true because the menu writes through
// writeClipboardText rather than a ClipboardEvent's clipboardData.
export function messageSelectionContextText(doc: Document, target: EventTarget | null): string | null {
  const selection = doc.getSelection();
  if (!targetWithinSelectedMessage(target, selection)) return null;
  return messageSelectionCopyText({
    text: restoreLatexInSelection(selection),
    isCollapsed: selection == null || selection.isCollapsed,
    targetIsEditable: isEditableTarget(target),
    intersectsMessage: selectionIntersectsMessage(selection, doc),
    canWriteClipboard: true,
  });
}

function targetWithinSelectedMessage(target: EventTarget | null, selection: Selection | null): boolean {
  const el = elementFromEventTarget(target);
  const message = el?.closest(MESSAGE_SELECTION_SELECTOR);
  if (!message || !selection || selection.rangeCount === 0) return false;
  for (let i = 0; i < selection.rangeCount; i += 1) {
    if (rangeIntersectsNode(selection.getRangeAt(i), message)) return true;
  }
  return false;
}

// Range.intersectsNode with a boundary-point fallback for DOM implementations
// that lack it (jsdom). Ranges intersect a node when the range starts before
// the node's end and ends after the node's start.
function rangeIntersectsNode(range: Range, node: Node): boolean {
  try {
    if (typeof range.intersectsNode === "function") return range.intersectsNode(node);
    const doc = node.ownerDocument;
    if (!doc) return false;
    const nodeRange = doc.createRange();
    nodeRange.selectNodeContents(node);
    return range.compareBoundaryPoints(nodeRange.END_TO_START, nodeRange) < 0
      && range.compareBoundaryPoints(nodeRange.START_TO_END, nodeRange) > 0;
  } catch {
    return false;
  }
}

export function installMessageSelectionCopy(doc: Document = document): () => void {
  const onCopy = (event: ClipboardEvent) => {
    const selection = doc.getSelection();
    const text = messageSelectionCopyText({
      text: restoreLatexInSelection(selection),
      isCollapsed: selection == null || selection.isCollapsed,
      targetIsEditable: isEditableTarget(event.target),
      intersectsMessage: selectionIntersectsMessage(selection, doc),
      canWriteClipboard: event.clipboardData != null,
    });
    if (text == null || event.clipboardData == null) return;

    event.clipboardData.setData("text/plain", text);
    event.preventDefault();
  };

  doc.addEventListener("copy", onCopy);
  return () => doc.removeEventListener("copy", onCopy);
}

/**
 * restoreLatexInSelection returns the clipboard text for the current
 * selection.  When the selection touches any KaTeX-rendered math element,
 * the rendered Unicode glyphs are replaced by the original LaTeX source
 * from the element's data-latex-source attribute.  This way Ctrl+C on a
 * math formula copies "\alpha" instead of "α".
 *
 * Strategy: we read the browser's selection.toString() as the base text
 * and then, for every .katex / .katex-display in the ORIGINAL DOM that
 * intersects the range, we replace that element's visible textContent
 * with its LaTeX source (wrapped in $…$ or $$…$$).  Working at the
 * string level avoids all the fragility of cloneContents() losing
 * MathML-namespaced annotations or inner DOM structure.
 */
function restoreLatexInSelection(selection: Selection | null): string {
  if (!selection || selection.isCollapsed || selection.rangeCount === 0) return "";
  const range = selection.getRangeAt(0);
  const baseText = selection.toString();
  if (baseText.trim() === "") return "";

  // Locate the enclosing message body so we only walk relevant KaTeX.
  const container = range.commonAncestorContainer;
  const root =
    container.nodeType === Node.ELEMENT_NODE
      ? (container as Element)
      : container.parentElement;
  if (!root) return baseText;
  const msgBody = root.closest(MESSAGE_SELECTION_SELECTOR);
  if (!msgBody) return baseText;

  // Collect every katex block that overlaps the selection.
  const allKatex =
    msgBody.querySelectorAll<HTMLElement>(".katex, .katex-display");
  interface KatexHit {
    rendered: string; // visible textContent of this element
    source: string;   // LaTeX to replace it with ($…$ or $$…$$)
  }
  const hits: KatexHit[] = [];

  for (const el of allKatex) {
    if (!range.intersectsNode(el)) continue;
    // Skip a bare .katex that lives inside a .katex-display we already captured.
    if (
      el.classList.contains("katex") &&
      el.closest(".katex-display")
    ) {
      const displayParent = el.closest(".katex-display") as HTMLElement;
      if (range.intersectsNode(displayParent)) continue;
    }
    const tex = el.getAttribute("data-latex-source");
    if (!tex) continue;
    const isDisplay = el.classList.contains("katex-display");
    const source = isDisplay ? `$$\n${tex}\n$$` : `$${tex}$`;
    const rendered = (el.textContent ?? "").replace(/[\u200B\u200C\u200D\uFEFF]/g, "");
    if (!rendered) continue;
    hits.push({ rendered, source });
  }

  if (hits.length === 0) return baseText;

  // Special case: the selection is entirely within a single katex block.
  // Return just the complete LaTeX source (the "auto-lock" behavior).
  if (hits.length === 1) {
    const sel = baseText.replace(/[\u200B\u200C\u200D\uFEFF]/g, "").trim();
    const renderedTrimmed = hits[0].rendered.trim();
    if (sel === "" || renderedTrimmed.includes(sel) || sel.includes(renderedTrimmed)) {
      return hits[0].source;
    }
  }

  // General case: replace each katex block's visible text with its
  // LaTeX source inside the base text.  We sort by length descending
  // so a longer rendered string (display math) is matched before a
  // shorter one that appears inside it.
  let text = baseText.replace(/[\u200B\u200C\u200D\uFEFF]/g, "");
  hits.sort((a, b) => b.rendered.length - a.rendered.length);
  for (const hit of hits) {
    // Escape the rendered string for use in a regex (math may contain
    // characters that are special in regex like \, ^, $, etc.).
    const escaped = hit.rendered.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    text = text.replace(new RegExp(escaped, "g"), hit.source);
  }
  return text;
}

// ---- Text extraction (after KaTeX → source substitution) ----------------

function selectionIntersectsMessage(selection: Selection | null, root: ParentNode): boolean {
  if (selection == null || selection.rangeCount === 0 || selection.isCollapsed) return false;
  for (let i = 0; i < selection.rangeCount; i += 1) {
    if (rangeIntersectsMessage(selection.getRangeAt(i), root)) return true;
  }
  return false;
}

function rangeIntersectsMessage(range: Range, root: ParentNode): boolean {
  const common = elementFromNode(range.commonAncestorContainer);
  const directMessage = common?.closest(MESSAGE_SELECTION_SELECTOR);
  if (directMessage) return true;

  const scope = common ?? root;
  const candidates = scope instanceof Element && scope.matches(MESSAGE_SELECTION_SELECTOR)
    ? [scope, ...Array.from(scope.querySelectorAll(MESSAGE_SELECTION_SELECTOR))]
    : Array.from(scope.querySelectorAll(MESSAGE_SELECTION_SELECTOR));

  return candidates.some((node) => rangeIntersectsNode(range, node));
}

function isEditableTarget(target: EventTarget | null): boolean {
  const el = elementFromEventTarget(target);
  if (!el) return false;
  if (el.closest("input, textarea, select")) return true;
  for (let node: Element | null = el; node; node = node.parentElement) {
    if (node instanceof HTMLElement && node.isContentEditable) return true;
  }
  return false;
}

function elementFromEventTarget(target: EventTarget | null): Element | null {
  return target instanceof Element ? target : null;
}

function elementFromNode(node: Node | null): Element | null {
  if (!node) return null;
  return node.nodeType === Node.ELEMENT_NODE ? node as Element : node.parentElement;
}
