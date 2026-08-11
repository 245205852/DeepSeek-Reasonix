import type { Item } from "./types";

/** Preserve transcript on failed hydrate instead of painting a successful empty session. */
export function applyHydrateErrorState<S extends {
  items: Item[];
  hydratePlaceholderItems?: Item[];
  hydrateHistoryLoaded?: boolean;
}>(s: S, reason: string, error: string): S & {
  hydrating: false;
  hydrateReason: string;
  hydrateError: string;
  hydrateHistoryLoaded?: boolean;
  hydratePlaceholderItems: undefined;
} {
  const keptItems = s.items.length > 0 ? s.items : (s.hydratePlaceholderItems?.length ? s.hydratePlaceholderItems : s.items);
  return {
    ...s,
    items: keptItems,
    hydrating: false,
    hydrateReason: reason,
    hydrateError: error,
    hydrateHistoryLoaded: s.hydrateHistoryLoaded || keptItems.length > 0 || undefined,
    hydratePlaceholderItems: undefined,
  };
}

export function hydratePlaceholderItems(optionsItems: Item[] | undefined, existing: Item[] | undefined): Item[] | undefined {
  if (optionsItems?.length) return optionsItems;
  return existing?.length ? existing : undefined;
}
