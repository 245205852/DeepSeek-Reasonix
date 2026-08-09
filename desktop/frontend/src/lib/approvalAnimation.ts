export type ShelfExitEase = "power2.in" | "power2.out";

export function shelfExitEasing(ease: ShelfExitEase): string {
  return ease === "power2.in"
    ? "cubic-bezier(0.55, 0.085, 0.68, 0.53)"
    : "cubic-bezier(0.2, 0.72, 0.2, 1)";
}

export function animateShelfExit(
  el: HTMLDivElement,
  options: { opacity: number; y: number; duration: number; ease: ShelfExitEase; onComplete: () => void },
) {
  let completed = false;
  const complete = () => {
    if (completed) return;
    completed = true;
    options.onComplete();
  };
  if (typeof el.animate !== "function") {
    complete();
    return;
  }
  try {
    const animation = el.animate(
      [
        { opacity: 1, transform: "translateY(0)" },
        { opacity: options.opacity, transform: `translateY(${options.y}px)` },
      ],
      { duration: options.duration * 1000, easing: shelfExitEasing(options.ease) },
    );
    animation.onfinish = complete;
    animation.oncancel = complete;
    void animation.finished.then(complete, complete);
  } catch {
    // Animation support is cosmetic; failures must not swallow the action.
    complete();
  }
}
