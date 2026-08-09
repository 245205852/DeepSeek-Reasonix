// Run: tsx src/__tests__/approval-animation.test.ts

import { animateShelfExit, shelfExitEasing } from "../components/ApprovalModal";

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

console.log("\napproval animation");

ok(shelfExitEasing("power2.in").startsWith("cubic-bezier("), "power2.in maps to a Web Animations easing");
ok(shelfExitEasing("power2.out").startsWith("cubic-bezier("), "power2.out maps to a Web Animations easing");

{
  let completed = 0;
  let capturedEasing = "";
  let resolveFinished: (() => void) | undefined;
  const finished = new Promise<void>((resolve) => { resolveFinished = resolve; });
  const animation = { finished, onfinish: null as (() => void) | null, oncancel: null as (() => void) | null };
  const el = {
    animate: (_frames: unknown, options: KeyframeAnimationOptions) => {
      capturedEasing = String(options.easing);
      return animation;
    },
  } as unknown as HTMLDivElement;

  animateShelfExit(el, { opacity: 0, y: 8, duration: 0.1, ease: "power2.in", onComplete: () => { completed += 1; } });
  animation.onfinish?.();
  animation.oncancel?.();
  resolveFinished?.();
  await finished;
  await Promise.resolve();

  ok(capturedEasing === shelfExitEasing("power2.in"), "animate receives the converted easing");
  ok(completed === 1, "finish, cancel, and finished promise complete the action once");
}

{
  let completed = 0;
  const el = {
    animate: () => { throw new TypeError("invalid easing"); },
  } as unknown as HTMLDivElement;
  animateShelfExit(el, { opacity: 0, y: 8, duration: 0.1, ease: "power2.in", onComplete: () => { completed += 1; } });
  ok(completed === 1, "a synchronous Web Animations failure still completes the action");
}

if (failed > 0) {
  console.error(`approval-animation: ${failed} failed, ${passed} passed`);
  process.exit(1);
}
console.log(`approval-animation: ${passed} passed`);
