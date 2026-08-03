// Run: tsx src/__tests__/provider-editor-model-picker.test.tsx

import { JSDOM } from "jsdom";
import React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { LocaleProvider } from "../lib/i18n";
import { ProviderEditorModelPicker } from "../components/SettingsPanel";

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

function flushPromises(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", {
  pretendToBeVisual: true,
  url: "http://localhost/",
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", { configurable: true, value: dom.window.navigator });
globalThis.Node = dom.window.Node;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.CustomEvent = dom.window.CustomEvent;
globalThis.KeyboardEvent = dom.window.KeyboardEvent;
globalThis.MouseEvent = dom.window.MouseEvent;
globalThis.localStorage = dom.window.localStorage;
globalThis.sessionStorage = dom.window.sessionStorage;
globalThis.requestAnimationFrame = dom.window.requestAnimationFrame.bind(dom.window);
globalThis.cancelAnimationFrame = dom.window.cancelAnimationFrame.bind(dom.window);
window.scrollTo = () => {};

function renderPicker(candidates: string[]) {
  return (
    <LocaleProvider>
      <ProviderEditorModelPicker
        candidates={candidates}
        selectedModels={[]}
        visionModels={[]}
        contextWindows={{}}
        disabled={false}
        onToggleModel={() => undefined}
        onToggleVision={() => undefined}
        onContextWindowChange={() => undefined}
        onSelectAll={() => undefined}
        onClear={() => undefined}
      />
    </LocaleProvider>
  );
}

console.log("\nprovider editor model picker");

const rootEl = document.getElementById("root");
if (!rootEl) throw new Error("missing root");
const root = createRoot(rootEl);

let threw = false;
try {
  await act(async () => {
    root.render(renderPicker([]));
    await flushPromises();
  });
  await act(async () => {
    root.render(renderPicker(["zen-v1", "zen-v1-pro"]));
    await flushPromises();
  });
} catch (error) {
  threw = true;
  process.stdout.write(`  ERROR ${String(error)}\n`);
}

ok(!threw, "model picker can render after async model fetch returns candidates");
ok(rootEl.textContent?.includes("zen-v1") === true, "model picker shows fetched custom provider models");

await act(async () => {
  root.unmount();
});
dom.window.close();

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);
