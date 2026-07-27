// Run: tsx src/__tests__/github-link-rendering.test.ts

import { classifyLinkIcon, parseGitHubLink } from "../components/githubLink";

let passed = 0;
let failed = 0;

function eq(actual: unknown, expected: unknown, label: string) {
  if (JSON.stringify(actual) === JSON.stringify(expected)) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(
      `  FAIL  ${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}\n`,
    );
    failed += 1;
  }
}

console.log("\ngithub link rendering");

eq(parseGitHubLink("https://github.com/esengine/DeepSeek-Reasonix/issues/6856"), {
  kind: "issue",
  owner: "esengine",
  repo: "DeepSeek-Reasonix",
  value: "6856",
  compactLabel: "#6856",
}, "recognizes GitHub issue links");

eq(parseGitHubLink("https://github.com/esengine/DeepSeek-Reasonix/pull/123?diff=split"), {
  kind: "pull",
  owner: "esengine",
  repo: "DeepSeek-Reasonix",
  value: "123",
  compactLabel: "PR #123",
}, "recognizes GitHub pull request links with query parameters");

eq(parseGitHubLink("https://github.com/esengine/DeepSeek-Reasonix/commit/abcdef1234567890"), {
  kind: "commit",
  owner: "esengine",
  repo: "DeepSeek-Reasonix",
  value: "abcdef1234567890",
  compactLabel: "abcdef1",
}, "recognizes GitHub commit links");

eq(parseGitHubLink("https://github.com/esengine/DeepSeek-Reasonix"), null, "leaves repository links unchanged");
eq(parseGitHubLink("http://github.com/esengine/DeepSeek-Reasonix/issues/1"), null, "rejects insecure GitHub links");
eq(parseGitHubLink("https://example.com/issues/6856"), null, "leaves non-GitHub links unchanged");
eq(parseGitHubLink("javascript:alert(1)"), null, "does not enhance unsafe links");

eq(classifyLinkIcon("https://github.com/esengine/DeepSeek-Reasonix"), "github", "adds a GitHub icon to repository links");
eq(classifyLinkIcon("https://example.com/docs"), "external", "adds an external-link icon to web links");
eq(classifyLinkIcon("http://localhost:5173/docs"), "external", "adds an external-link icon to HTTP links");
eq(classifyLinkIcon("mailto:hello@example.com"), "mail", "adds a mail icon to email links");
eq(classifyLinkIcon("./docs/GUIDE.md"), null, "leaves relative links without an icon");
eq(classifyLinkIcon("#section"), null, "leaves page fragments without an icon");
eq(classifyLinkIcon("javascript:alert(1)"), null, "does not add icons to unsafe protocols");

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);
