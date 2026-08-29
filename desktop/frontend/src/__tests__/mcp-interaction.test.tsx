// Run: tsx src/__tests__/mcp-interaction.test.tsx
// MCP elicitation: wire event → reducer state, StructuredForm schema
// normalization/coercion, MCPInteractionCard rendering and answer wiring.

import { JSDOM } from "jsdom";
import { registerHooks } from "node:module";
import React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.endsWith(".css") || specifier.endsWith(".svg")) {
      return nextResolve("./asset-stub-for-tests.ts", { ...context, parentURL: import.meta.url });
    }
    return nextResolve(specifier, context);
  },
});

import { LocaleProvider } from "../lib/i18n";
import type { WireEvent } from "../lib/types";
import { initialState, reducer } from "../lib/useController";
import { MCPInteractionCard } from "../components/MCPInteractionCard";
import {
  coerceStructuredValues,
  initialStructuredValues,
  missingStructuredRequired,
  normalizeStructuredSchema,
} from "../components/StructuredForm";

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

type ControllerState = Parameters<typeof reducer>[0];

const dom = new JSDOM("<!doctype html><html><body><div id='root'></div></body></html>");
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

// ── Schema normalization ─────────────────────────────────────────────────────

{
  const fields = normalizeStructuredSchema({
    type: "object",
    required: ["code", "count"],
    properties: {
      code: { type: "string", title: "Device code", minLength: 4, maxLength: 8 },
      count: { type: "integer", minimum: 1, maximum: 5, default: 2 },
      ok: { type: "boolean" },
      flavor: { enum: ["vanilla", "mint"] },
    },
  });
  ok(fields.length === 4, "all flat properties become fields");
  const code = fields.find((f) => f.key === "code");
  ok(code?.kind === "string" && code.required && code.minLength === 4, "string field carries required + bounds");
  const count = fields.find((f) => f.key === "count");
  ok(count?.kind === "integer" && count.defaultValue === "2" && count.maximum === 5, "integer field carries default + max");
  ok(fields.find((f) => f.key === "ok")?.kind === "boolean", "boolean field detected");
  ok(fields.find((f) => f.key === "flavor")?.kind === "enum", "enum field detected");
  ok(normalizeStructuredSchema(null).length === 0, "null schema yields no fields");
  ok(normalizeStructuredSchema({}).length === 0, "schema without properties yields no fields");
}

// ── Value coercion ───────────────────────────────────────────────────────────

{
  const fields = normalizeStructuredSchema({
    type: "object",
    required: ["name"],
    properties: {
      name: { type: "string", minLength: 2 },
      age: { type: "integer" },
      pi: { type: "number" },
      ok: { type: "boolean", default: true },
    },
  });
  const defaults = initialStructuredValues(fields);
  ok(defaults.ok === true, "boolean default stays explicit");
  ok(missingStructuredRequired(fields, defaults)[0] === "name", "missing required reported by label");
  const { content, invalid } = coerceStructuredValues(fields, {
    ...defaults,
    name: "jo",
    age: "41",
    pi: "3.5",
  });
  ok(invalid.length === 0, "valid values coerce without errors");
  ok(content.name === "jo" && content.age === 41 && content.pi === 3.5 && content.ok === true, "types coerced to JSON types");
  const bad = coerceStructuredValues(fields, { ...defaults, name: "j", age: "not-a-number" });
  ok(bad.invalid.includes("name") && bad.invalid.includes("age"), "bound violations and NaN reported");
}

// ── Reducer ──────────────────────────────────────────────────────────────────

{
  const event: WireEvent = {
    kind: "mcp_interaction",
    turnId: "t1",
    itemId: "42",
    mcpInteraction: {
      id: "42",
      server: "github",
      mode: "form",
      message: "confirm",
      requestedSchema: { type: "object", properties: { code: { type: "string" } }, required: ["code"] },
    },
  } as unknown as WireEvent;
  const next = reducer({ ...initialState }, { type: "event", e: event } as never);
  ok((next as ControllerState).mcpInteraction?.id === "42", "mcp_interaction event sets state");
  ok((next as ControllerState).pendingPrompt === true, "mcp_interaction waits for the user");

  const answered = reducer(next, {
    type: "event",
    e: { kind: "prompt_answered", turnId: "t1", itemId: "42" } as unknown as WireEvent,
  } as never);
  ok((answered as ControllerState).mcpInteraction === undefined, "prompt_answered clears the card");
}

// ── Card rendering + submit wiring ───────────────────────────────────────────

{
  const answers: { id: string; action: string; content?: Record<string, unknown> }[] = [];
  const root = createRoot(document.getElementById("root")!);
  await act(async () => {
    root.render(
      <LocaleProvider>
        <MCPInteractionCard
          interaction={{
            id: "7",
            server: "github",
            mode: "form",
            message: "Enter the device code",
            requestedSchema: {
              type: "object",
              required: ["code"],
              properties: { code: { type: "string", title: "Device code", default: "123-456" } },
            },
          }}
          busy={false}
          onAnswer={(id, action, content) => answers.push({ id, action, content })}
        />
      </LocaleProvider>,
    );
  });
  const text = document.body.textContent ?? "";
  ok(text.includes("github") && text.includes("Device code"), "card shows server and field label");

  const input = document.querySelector(".structured-form-input") as HTMLInputElement | null;
  ok(input !== null, "form field rendered as input");
  // jsdom cannot reliably drive React controlled-input keystrokes; the default
  // value prefills the field so the accept path is exercised end-to-end, and
  // typed-value coercion is covered above.
  ok(input?.value === "123-456", "required field prefilled from schema default");
  const submit = Array.from(document.querySelectorAll("button, [role='button']")).find((b) =>
    (b.textContent ?? "").toLowerCase().includes("submit"),
  );
  ok(submit !== undefined, "submit action rendered");
  if (submit) {
    await act(async () => {
      submit.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
  }

  ok(
    answers.length === 1 && answers[0].action === "accept" && (answers[0].content as Record<string, unknown>)?.code === "123-456",
    "submit sends accept with the typed form values",
  );
  await act(async () => {
    root.unmount();
  });
}

// ── URL card ─────────────────────────────────────────────────────────────────

{
  const answers: { id: string; action: string; content?: Record<string, unknown> }[] = [];
  const opened: string[] = [];
  const root = createRoot(document.getElementById("root")!);
  await act(async () => {
    root.render(
      <LocaleProvider>
        <MCPInteractionCard
          interaction={{
            id: "9",
            server: "linear",
            mode: "url",
            message: "Finish sign-in",
            url: "https://auth.example.com/cb?state=xyz",
          }}
          busy={false}
          onAnswer={(id, action, content) => answers.push({ id, action, content })}
          onOpenLink={(url) => opened.push(url)}
        />
      </LocaleProvider>,
    );
  });
  const text = document.body.textContent ?? "";
  const urlSummary = document.querySelector(".mcp-interaction-url")?.textContent ?? "";
  ok(urlSummary.trim() === "linear → auth.example.com", "url card shows the exact server and target host");
  ok(!text.includes("?state=xyz"), "url card hides query params from the summary line");
  const open = document.querySelector(".prompt-shelf-bar-actions button");
  if (open) {
    await act(async () => {
      open.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
  }
  ok(opened.length === 1 && opened[0] === "https://auth.example.com/cb?state=xyz", "open link passes the exact URL once");
  const accept = Array.from(document.querySelectorAll("button, [role='button']")).find((b) =>
    (b.textContent ?? "").toLowerCase() === "accept",
  );
  if (accept) {
    await act(async () => {
      accept.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
  }
  ok(answers.length === 1 && answers[0].action === "accept" && answers[0].content === undefined, "accept without form content");
  await act(async () => {
    root.unmount();
  });
}

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
