import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useT } from "../lib/i18n";

export function PromptDescriptionToggle({
  descriptionId,
  expanded,
  onToggle,
  disabled = false,
}: {
  descriptionId: string;
  expanded: boolean;
  onToggle: () => void;
  disabled?: boolean;
}) {
  const t = useT();
  return (
    <button
      type="button"
      className="prompt-action__description-toggle"
      aria-expanded={expanded}
      aria-controls={descriptionId}
      onClick={onToggle}
      onKeyDown={(event) => {
        // Decision surfaces also own document-level Enter shortcuts. Keep
        // disclosure activation local so the same key press cannot confirm
        // the currently selected decision.
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        event.stopPropagation();
        onToggle();
      }}
      disabled={disabled}
    >
      {t(expanded ? "decision.hideFullDescription" : "decision.showFullDescription")}
    </button>
  );
}

export function PromptDescriptionDisclosure({
  descriptionId,
  label,
  description,
  expanded,
  onToggle,
  disabled = false,
  alwaysVisible = false,
}: {
  descriptionId: string;
  label?: ReactNode;
  description: ReactNode;
  expanded: boolean;
  onToggle?: () => void;
  disabled?: boolean;
  alwaysVisible?: boolean;
}) {
  const visible = alwaysVisible || expanded;
  return (
    <div className="prompt-description-disclosure">
      {!alwaysVisible && onToggle && (
        <PromptDescriptionToggle
          descriptionId={descriptionId}
          expanded={expanded}
          onToggle={onToggle}
          disabled={disabled}
        />
      )}
      <div
        id={descriptionId}
        className="prompt-description-detail"
        role="region"
        hidden={!visible}
      >
        {label != null && label !== "" && (
          <strong className="prompt-description-detail__label">{label}</strong>
        )}
        <span className="prompt-description-detail__text">{description}</span>
      </div>
    </div>
  );
}

export function PromptAction({
  actionId,
  keyLabel,
  label,
  description,
  descriptionId,
  descriptionDisclosure = false,
  descriptionAlwaysVisible = false,
  onDescriptionOverflowChange,
  onClick,
  ariaLabel,
  title,
  onHoverChange,
  primary = false,
  selected = false,
  // Keyboard cursor without implying a committed answer (multi-select).
  active = false,
  quiet = false,
  disabled = false,
  tone = "default",
  role = "option",
}: {
  actionId?: string;
  keyLabel: string;
  label?: ReactNode;
  description?: ReactNode;
  descriptionId?: string;
  // Keep supplementary copy to one stable summary line and reveal the complete
  // text in a separate detail region. Option/listbox surfaces render that
  // region outside the listbox; immediate button groups own it here.
  descriptionDisclosure?: boolean;
  // Safety-critical consequences open automatically when the stable summary
  // row is actually truncated. Complete summaries are not repeated.
  descriptionAlwaysVisible?: boolean;
  onDescriptionOverflowChange?: (overflowing: boolean) => void;
  onClick: () => void;
  ariaLabel?: string;
  // Native tooltip is a desktop convenience; the disclosure remains the
  // keyboard/touch accessible path to the complete description.
  title?: string;
  // Fires on mouse enter/focus (true) and mouse leave/blur (false) so the
  // parent can drive a focus-following detail preview.
  onHoverChange?: (hovering: boolean) => void;
  primary?: boolean;
  selected?: boolean;
  active?: boolean;
  quiet?: boolean;
  disabled?: boolean;
  // Danger options (deny / clear) use semantic color but are never default-selected.
  tone?: "default" | "danger";
  role?: "option" | "button";
}) {
  const generatedDescriptionId = useId();
  const actionRef = useRef<HTMLButtonElement | null>(null);
  const descriptionRef = useRef<HTMLSpanElement | null>(null);
  const overflowCallbackRef = useRef(onDescriptionOverflowChange);
  const [internalExpanded, setInternalExpanded] = useState(false);
  const [descriptionOverflowing, setDescriptionOverflowing] = useState(false);
  overflowCallbackRef.current = onDescriptionOverflowChange;

  const hasCopy = description != null || (label != null && label !== "");
  const resolvedDescriptionId = description
    ? (descriptionId ?? `${generatedDescriptionId}-description`)
    : undefined;
  const detailDescriptionId = resolvedDescriptionId ? `${resolvedDescriptionId}-detail` : undefined;
  const expanded = internalExpanded;
  const descriptionText = typeof description === "string" ? description : undefined;
  const resolvedTitle = title ?? (descriptionDisclosure ? descriptionText : undefined);

  useEffect(() => {
    setInternalExpanded(false);
  }, [description]);

  useLayoutEffect(() => {
    if (!descriptionDisclosure || !descriptionRef.current) {
      setDescriptionOverflowing(false);
      overflowCallbackRef.current?.(false);
      return;
    }
    // Once expanded, retain the known overflow state so the Collapse action
    // remains available even though the unclamped element no longer overflows.
    if (expanded) return;

    const element = descriptionRef.current;
    const measure = () => {
      const overflowing = element.scrollHeight > element.clientHeight + 1
        || element.scrollWidth > element.clientWidth + 1;
      setDescriptionOverflowing((current) => current === overflowing ? current : overflowing);
      overflowCallbackRef.current?.(overflowing);
    };
    measure();
    // The shelf can finish constraining its grid after this layout effect.
    // Recheck over the next two frames so an initially content-sized summary
    // cannot hide the disclosure once the final row width is known.
    let cancelled = false;
    let settledFrame = 0;
    const layoutFrame = window.requestAnimationFrame(() => {
      measure();
      settledFrame = window.requestAnimationFrame(measure);
    });
    void document.fonts?.ready.then(() => {
      if (!cancelled) measure();
    });
    const observer = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    observer?.observe(element);
    window.addEventListener("resize", measure);
    return () => {
      cancelled = true;
      window.cancelAnimationFrame(layoutFrame);
      if (settledFrame) window.cancelAnimationFrame(settledFrame);
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, [descriptionDisclosure, expanded, resolvedDescriptionId, description, Boolean(onDescriptionOverflowChange)]);

  useEffect(() => {
    if (!selected && !active && !expanded) return;
    actionRef.current?.scrollIntoView?.({ block: "nearest" });
  }, [active, expanded, selected]);

  const action = (
    <button
      ref={actionRef}
      id={actionId}
      type="button"
      role={role}
      aria-selected={role === "option" ? selected : undefined}
      data-active={active ? "true" : undefined}
      className={[
        "prompt-action",
        primary || selected ? " prompt-action--selected" : "",
        active ? " prompt-action--active" : "",
        quiet ? " prompt-action--quiet" : "",
        description ? " prompt-action--descriptive" : "",
        descriptionDisclosure ? " prompt-action--description-collapsible" : "",
        !hasCopy ? " prompt-action--key-only" : "",
        tone === "danger" ? " prompt-action--danger" : "",
      ].join("")}
      onClick={onClick}
      disabled={disabled}
      aria-label={ariaLabel}
      title={resolvedTitle}
      onMouseEnter={onHoverChange ? () => onHoverChange(true) : undefined}
      onMouseLeave={onHoverChange ? () => onHoverChange(false) : undefined}
      onFocus={onHoverChange ? () => onHoverChange(true) : undefined}
      onBlur={onHoverChange ? () => onHoverChange(false) : undefined}
    >
      {keyLabel && <span className="prompt-action__key">{keyLabel}</span>}
      {hasCopy && (
        <span className="prompt-action__copy">
          {label != null && label !== "" && <span className="prompt-action__label">{label}</span>}
          {description && (
            <span ref={descriptionRef} id={resolvedDescriptionId} className="prompt-action__desc">
              {description}
            </span>
          )}
        </span>
      )}
    </button>
  );

  // A listbox may only own options, so its explicit disclosure is rendered by
  // the parent in PromptShelf.note. Button groups can safely keep the control
  // next to the corresponding immediate action.
  if (role !== "button" || (!descriptionDisclosure && !descriptionAlwaysVisible)) return action;

  return (
    <div className={[
      "prompt-action-row",
      expanded || (descriptionAlwaysVisible && descriptionOverflowing) ? " prompt-action-row--description-expanded" : "",
    ].join("")}>
      {action}
      {detailDescriptionId && description && descriptionOverflowing && (
        <PromptDescriptionDisclosure
          descriptionId={detailDescriptionId}
          label={label}
          description={description}
          expanded={expanded}
          onToggle={() => setInternalExpanded((current) => !current)}
          disabled={disabled}
          alwaysVisible={descriptionAlwaysVisible}
        />
      )}
    </div>
  );
}
