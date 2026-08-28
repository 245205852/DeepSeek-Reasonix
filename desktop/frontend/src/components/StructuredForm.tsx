import { useMemo, useState } from "react";
import { useI18n } from "../lib/i18n";

// One field of a flat primitive elicitation schema after normalization.
export type StructuredField = {
  key: string;
  label: string;
  kind: "string" | "number" | "integer" | "boolean" | "enum";
  required: boolean;
  defaultValue?: string;
  options?: { label: string; value: string }[];
  minLength?: number;
  maxLength?: number;
  minimum?: number;
  maximum?: number;
  description?: string;
};

export type StructuredFieldValue = string | number | boolean;

// normalizeStructuredSchema flattens an elicitation requestedSchema (flat
// primitive properties only per the MCP spec) into renderable fields. Unknown
// or nested shapes degrade to a plain string field rather than blocking the
// prompt.
export function normalizeStructuredSchema(schema: unknown): StructuredField[] {
  if (!schema || typeof schema !== "object") return [];
  const props = (schema as { properties?: Record<string, unknown> }).properties;
  if (!props || typeof props !== "object") return [];
  const required = new Set(
    Array.isArray((schema as { required?: unknown[] }).required)
      ? ((schema as { required?: unknown[] }).required as unknown[]).filter((v): v is string => typeof v === "string")
      : [],
  );
  const fields: StructuredField[] = [];
  for (const [key, raw] of Object.entries(props)) {
    if (!raw || typeof raw !== "object") continue;
    const prop = raw as Record<string, unknown>;
    const enumValues = Array.isArray(prop.enum)
      ? (prop.enum as unknown[]).filter((v) => v !== null && v !== undefined)
      : null;
    let kind: StructuredField["kind"] = "string";
    if (enumValues && enumValues.length > 0) kind = "enum";
    else if (typeof prop.type === "string") {
      if (prop.type === "number" || prop.type === "integer" || prop.type === "boolean" || prop.type === "string") {
        kind = prop.type;
      }
    }
    const defaultValue =
      prop.default === null || prop.default === undefined
        ? undefined
        : kind === "boolean"
          ? prop.default
            ? "true"
            : "false"
          : String(prop.default);
    fields.push({
      key,
      label: typeof prop.title === "string" && prop.title ? prop.title : key,
      kind,
      required: required.has(key),
      defaultValue,
      options: enumValues
        ? enumValues.map((v) => ({
            label: String(v),
            value: typeof v === "string" ? v : JSON.stringify(v),
          }))
        : undefined,
      minLength: typeof prop.minLength === "number" ? prop.minLength : undefined,
      maxLength: typeof prop.maxLength === "number" ? prop.maxLength : undefined,
      minimum: typeof prop.minimum === "number" ? prop.minimum : undefined,
      maximum: typeof prop.maximum === "number" ? prop.maximum : undefined,
      description: typeof prop.description === "string" ? prop.description : undefined,
    });
  }
  return fields;
}

// initialStructuredValues applies defaults (and boolean false defaults stay
// explicit so required booleans do not block submission).
export function initialStructuredValues(fields: StructuredField[]): Record<string, StructuredFieldValue> {
  const values: Record<string, StructuredFieldValue> = {};
  for (const field of fields) {
    if (field.defaultValue === undefined) continue;
    values[field.key] = field.kind === "boolean" ? field.defaultValue === "true" : field.defaultValue;
  }
  return values;
}

export function missingStructuredRequired(
  fields: StructuredField[],
  values: Record<string, StructuredFieldValue>,
): string[] {
  return fields.filter((f) => f.required && values[f.key] === undefined).map((f) => f.label);
}

// coerceStructuredValues converts the string form entries into the JSON types
// the schema asks for; unparseable numbers are reported instead of sent.
export function coerceStructuredValues(
  fields: StructuredField[],
  values: Record<string, StructuredFieldValue>,
): { content: Record<string, unknown>; invalid: string[] } {
  const content: Record<string, unknown> = {};
  const invalid: string[] = [];
  for (const field of fields) {
    const value = values[field.key];
    if (value === undefined || value === "") continue;
    if (field.kind === "number" || field.kind === "integer") {
      const num = field.kind === "integer" ? Number.parseInt(String(value), 10) : Number(String(value));
      if (!Number.isFinite(num)) {
        invalid.push(field.label);
        continue;
      }
      if (field.minimum !== undefined && num < field.minimum) invalid.push(field.label);
      if (field.maximum !== undefined && num > field.maximum) invalid.push(field.label);
      content[field.key] = num;
      continue;
    }
    if (field.kind === "boolean") {
      content[field.key] = value === true || value === "true";
      continue;
    }
    const text = String(value);
    if (field.minLength !== undefined && text.length < field.minLength) invalid.push(field.label);
    if (field.maxLength !== undefined && text.length > field.maxLength) invalid.push(field.label);
    content[field.key] = text;
  }
  return { content, invalid };
}

// StructuredForm renders flat primitive schema fields. It owns no submission
// protocol: the caller collects values and submits. MCP elicitation and
// extension forms share it with different submit paths.
export function StructuredForm({
  fields,
  values,
  onChange,
  disabled,
}: {
  fields: StructuredField[];
  values: Record<string, StructuredFieldValue>;
  onChange: (next: Record<string, StructuredFieldValue>) => void;
  disabled?: boolean;
}) {
  const { t } = useI18n();
  const [focusKey, setFocusKey] = useState<string | null>(null);
  const orderedKeys = useMemo(() => fields.map((f) => f.key), [fields]);

  const set = (key: string, value: StructuredFieldValue | undefined) => {
    const next = { ...values };
    if (value === undefined) delete next[key];
    else next[key] = value;
    onChange(next);
  };

  return (
    <div className="structured-form" role="list">
      {fields.map((field) => {
        const raw = values[field.key];
        const stringRaw = raw === undefined ? "" : String(raw);
        const invalid =
          raw !== undefined &&
          raw !== "" &&
          ((field.minLength !== undefined && stringRaw.length < field.minLength) ||
            (field.maxLength !== undefined && stringRaw.length > field.maxLength));
        const focused = focusKey === field.key;
        void focused;
        return (
          <label key={field.key} className="structured-form-field" data-index={orderedKeys.indexOf(field.key)}>
            <span className="structured-form-label">
              {field.label}
              {field.required ? <span className="structured-form-required" aria-hidden="true"> *</span> : null}
            </span>
            {field.description ? <span className="structured-form-hint">{field.description}</span> : null}
            {field.kind === "boolean" ? (
              <input
                type="checkbox"
                className="structured-form-checkbox"
                checked={raw === true || raw === "true"}
                disabled={disabled}
                onChange={(e) => set(field.key, e.target.checked)}
              />
            ) : field.kind === "enum" && field.options ? (
              <select
                className="structured-form-select"
                value={stringRaw}
                disabled={disabled}
                onChange={(e) => set(field.key, e.target.value === "" ? undefined : e.target.value)}
              >
                <option value="">{t("mcp.interaction.chooseOption")}</option>
                {field.options.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </select>
            ) : (
              <input
                type={field.kind === "number" || field.kind === "integer" ? "number" : "text"}
                className={invalid ? "structured-form-input is-invalid" : "structured-form-input"}
                value={stringRaw}
                disabled={disabled}
                min={field.minimum}
                max={field.maximum}
                minLength={field.minLength}
                maxLength={field.maxLength}
                placeholder={field.defaultValue ?? ""}
                onFocus={() => setFocusKey(field.key)}
                onBlur={() => setFocusKey(null)}
                onChange={(e) => set(field.key, e.target.value === "" ? undefined : e.target.value)}
              />
            )}
          </label>
        );
      })}
    </div>
  );
}
