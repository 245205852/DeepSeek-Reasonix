import { useMemo, useState } from "react";
import type { WireMCPInteraction } from "../lib/types";
import { useI18n } from "../lib/i18n";
import { PromptShelf, PromptBadge } from "./PromptShelf";
import { PromptAction } from "./PromptAction";
import {
  StructuredForm,
  coerceStructuredValues,
  initialStructuredValues,
  missingStructuredRequired,
  normalizeStructuredSchema,
  type StructuredFieldValue,
} from "./StructuredForm";

// hostOfURL extracts the display origin for url-mode confirmations.
function hostOfURL(raw: string | undefined): string {
  if (!raw) return "";
  try {
    return new URL(raw).host;
  } catch {
    return raw;
  }
}

// MCPInteractionCard renders one server-initiated elicitation: a typed form
// (flat primitive schema) or a URL confirmation. Submit/explicit refuse/close
// map to accept/decline/cancel.
export function MCPInteractionCard({
  interaction,
  busy,
  onAnswer,
  onOpenLink,
}: {
  interaction: WireMCPInteraction;
  busy: boolean;
  onAnswer: (id: string, action: "accept" | "decline" | "cancel", content?: Record<string, unknown>) => void;
  onOpenLink?: (url: string) => void;

}) {
  const { t } = useI18n();
  const fields = useMemo(() => normalizeStructuredSchema(interaction.requestedSchema), [interaction.requestedSchema]);
  const [values, setValues] = useState<Record<string, StructuredFieldValue>>(() => initialStructuredValues(fields));
  const [openedLink, setOpenedLink] = useState(false);

  const missing = missingStructuredRequired(fields, values);
  const { invalid } = coerceStructuredValues(fields, values);
  const formBlocked = missing.length > 0 || invalid.length > 0;

  const submit = () => {
    const { content } = coerceStructuredValues(fields, values);
    onAnswer(interaction.id, "accept", content);
  };

  const openLink = () => {
    setOpenedLink(true);
    if (interaction.url) onOpenLink?.(interaction.url);
  };

  return (
    <PromptShelf
      className="mcp-interaction-shelf"
      titleId={`mcp-interaction-${interaction.id}`}
      title={t("mcp.interaction.title")}
      badges={
        <>
          <PromptBadge>{interaction.server}</PromptBadge>
          <PromptBadge>
            {interaction.mode === "url" ? t("mcp.interaction.modeUrl") : t("mcp.interaction.modeForm")}
          </PromptBadge>
        </>
      }
      meta={interaction.message}
      decision
      footer={
        interaction.mode === "url" ? (
          <div className="prompt-shelf-bar">
            <span className="prompt-shelf-bar-hint">
              {openedLink ? t("mcp.interaction.urlOpenedHint") : t("mcp.interaction.urlHint")}
            </span>
            <div className="prompt-shelf-bar-actions">
              <PromptAction
                keyLabel=""
                label={t("mcp.interaction.openUrl", { host: hostOfURL(interaction.url ?? "") })}
                onClick={openLink}
                disabled={busy}
              />
              <PromptAction keyLabel="" label={t("mcp.interaction.accept")} onClick={() => onAnswer(interaction.id, "accept")} disabled={busy} primary />
              <PromptAction keyLabel="" label={t("mcp.interaction.decline")} onClick={() => onAnswer(interaction.id, "decline")} disabled={busy} />
            </div>
          </div>
        ) : (
          <div className="prompt-shelf-bar">
            <span className="prompt-shelf-bar-hint">
              {invalid.length > 0
                ? t("mcp.interaction.invalidValue", { label: invalid[0] })
                : missing.length > 0
                  ? t("mcp.interaction.required", { label: missing[0] })
                  : " "}
            </span>
            <div className="prompt-shelf-bar-actions">
              <PromptAction keyLabel="" label={t("mcp.interaction.submit")} onClick={submit} disabled={busy || formBlocked} primary />
              <PromptAction keyLabel="" label={t("mcp.interaction.decline")} onClick={() => onAnswer(interaction.id, "decline")} disabled={busy} />
              <PromptAction keyLabel="" label={t("mcp.interaction.cancel")} onClick={() => onAnswer(interaction.id, "cancel")} disabled={busy} />
            </div>
          </div>
        )
      }
    >
      {interaction.mode === "url" ? (
        <p className="mcp-interaction-url">
          {interaction.server} → {hostOfURL(interaction.url)}
        </p>
      ) : fields.length > 0 ? (
        <StructuredForm fields={fields} values={values} onChange={setValues} disabled={busy} />
      ) : (
        <p className="mcp-interaction-noschema">{t("mcp.interaction.confirmOnly")}</p>
      )}
    </PromptShelf>
  );
}
