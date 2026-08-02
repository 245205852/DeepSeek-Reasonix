import { useEffect, useRef } from "react";
import { PromptAction, PromptBadge, PromptShelf } from "./PromptShelf";

export type RuntimeDecisionAction = {
  key: string;
  label: string;
  description: string;
  onClick: () => void;
  danger?: boolean;
  disabled?: boolean;
};

export function RuntimeDecisionCard({
  id,
  title,
  badge,
  meta,
  note,
  actions,
  onCancel,
}: {
  id: string;
  title: string;
  badge: string;
  meta: string;
  note?: string;
  actions: RuntimeDecisionAction[];
  onCancel: () => void;
}) {
  const shelfRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    shelfRef.current?.focus();
    const onKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape") return;
      const target = event.target instanceof HTMLElement ? event.target : null;
      if (target?.matches("input, textarea, [contenteditable=true]")) return;
      event.preventDefault();
      onCancel();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [onCancel]);

  return (
    <PromptShelf
      decision
      barRef={shelfRef}
      titleId={`${id}-title`}
      title={title}
      badges={<PromptBadge tone="amber">{badge}</PromptBadge>}
      meta={meta}
      actionsRole="group"
      actions={actions.map((action) => (
        <PromptAction
          key={action.key}
          keyLabel={action.key}
          label={action.label}
          description={action.description}
          onClick={action.onClick}
          disabled={action.disabled}
          tone={action.danger ? "danger" : "default"}
          role="button"
        />
      ))}
    >
      {note && <p className="prompt-shelf__note">{note}</p>}
    </PromptShelf>
  );
}
