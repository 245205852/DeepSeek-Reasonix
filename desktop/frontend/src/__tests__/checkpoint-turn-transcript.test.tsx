// Run: tsx src/__tests__/checkpoint-turn-transcript.test.tsx

import { JSDOM } from "jsdom";
import { register } from "node:module";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import gsap from "gsap";
import { LocaleProvider } from "../lib/i18n";
import { initialState, reducer } from "../lib/useController";

register(new URL("../../scripts/svg-loader.mjs", import.meta.url));
const { Transcript } = await import("../components/Transcript");

type GsapOptions = { onComplete?: () => void };
const gsapForTests = gsap as unknown as {
  to: (_target: unknown, options: GsapOptions) => unknown;
  fromTo: (_target: unknown, _from: unknown, options: GsapOptions) => unknown;
  set: () => unknown;
  killTweensOf: () => void;
};
gsapForTests.to = (_target, options) => { options.onComplete?.(); return {}; };
gsapForTests.fromTo = (_target, _from, options) => { options.onComplete?.(); return {}; };
gsapForTests.set = () => ({});
gsapForTests.killTweensOf = () => {};

let passed = 0;
let failed = 0;

function ok(value: boolean, label: string) {
  if (value) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

function eq(actual: unknown, expected: unknown, label: string) {
  ok(actual === expected, `${label}${actual === expected ? "" : `: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`}`);
}

function flushTimers(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

class TestResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

console.log("\nturn checkpoint transcript integration");

const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", {
  pretendToBeVisual: true,
  url: "http://localhost/",
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", { configurable: true, value: dom.window.navigator });
globalThis.Node = dom.window.Node;
globalThis.Element = dom.window.Element;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.HTMLTextAreaElement = dom.window.HTMLTextAreaElement;
globalThis.Event = dom.window.Event;
globalThis.MouseEvent = dom.window.MouseEvent;
globalThis.MutationObserver = dom.window.MutationObserver;
globalThis.localStorage = dom.window.localStorage;
globalThis.requestAnimationFrame = dom.window.requestAnimationFrame.bind(dom.window);
globalThis.cancelAnimationFrame = dom.window.cancelAnimationFrame.bind(dom.window);
globalThis.ResizeObserver = TestResizeObserver;
Object.defineProperty(dom.window.HTMLElement.prototype, "attachEvent", { configurable: true, value: () => {} });
Object.defineProperty(dom.window.HTMLElement.prototype, "detachEvent", { configurable: true, value: () => {} });
Object.defineProperty(dom.window.HTMLElement.prototype, "scrollIntoView", { configurable: true, value: () => {} });
Object.defineProperty(window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: true, media: "(prefers-reduced-motion: reduce)", addEventListener() {}, removeEventListener() {} }),
});

let state = reducer(initialState, {
  type: "history_page",
  mode: "replace",
  page: {
    messages: [
      { role: "user", content: "existing prompt", checkpointTurn: 0 },
    ],
    startTurn: 0,
    endTurn: 1,
    totalTurns: 1,
    hasOlder: false,
  },
});
const cancelledSubmissionId = "transcript-cancelled-submission";
state = reducer(state, { type: "user", text: "cancelled prompt", seq: state.seq, submissionId: cancelledSubmissionId });
state = reducer(state, { type: "unsend" });
state = reducer(state, { type: "event", e: { kind: "turn_done", err: "context canceled", checkpointTurn: 1, submissionId: cancelledSubmissionId } });
state = reducer(state, {
  type: "checkpoints",
  checkpoints: [
    { turn: 0, prompt: "existing prompt", files: [], time: 1, canConversation: true },
    { turn: 1, prompt: "cancelled prompt", files: [], time: 2, canConversation: true },
  ],
});

const editTargets: number[] = [];
const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("missing root");
const root = createRoot(rootElement);
await act(async () => {
  root.render(
    <LocaleProvider>
      <Transcript
        items={state.items}
        checkpoints={state.checkpoints}
        questionNavigator={false}
        onPrompt={() => {}}
        onEditPrompt={(turn) => { editTargets.push(turn); return true; }}
      />
    </LocaleProvider>,
  );
  await flushTimers();
});

const cancelledMessage = Array.from(document.querySelectorAll<HTMLElement>(".msg--user"))
  .find((element) => element.textContent?.includes("cancelled prompt"));
eq(cancelledMessage?.dataset.turn, "1", "Transcript maps the cancelled user to checkpoint turn 1");
const editButton = cancelledMessage?.querySelector<HTMLButtonElement>('button[aria-label="Edit"]');
eq(editButton?.disabled, false, "checkpoint metadata enables Edit for the cancelled turn");

await act(async () => {
  editButton?.click();
  await flushTimers();
});
const editForm = cancelledMessage?.querySelector<HTMLFormElement>("form.msg-edit");
await act(async () => {
  editForm?.dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
  await flushTimers();
});
eq(editTargets[0], 1, "inline resend targets the authoritative checkpoint turn 1");

await act(async () => root.unmount());
dom.window.close();

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);
