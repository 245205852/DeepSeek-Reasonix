import { lazy, memo, Suspense, useMemo, useRef } from "react";
import type { ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import type { Components } from "react-markdown";
import { ExternalLink, Mail } from "lucide-react";
import remarkGfm from "remark-gfm";
import remarkMath from "remark-math";
import rehypeKatex from "rehype-katex";
import "katex/dist/katex.min.css";
import { CodeViewer } from "./CodeViewer";
import { classifyLinkIcon, parseGitHubLink, type GitHubLinkInfo, type LinkIconKind } from "./githubLink";
import { normalizeMath } from "./mathNormalize";
import { openExternal } from "../lib/bridge";
import { markdownImageSource } from "../lib/markdownImage";

const MermaidDiagram = lazy(() => import("./MermaidDiagram"));

// Markdown rendering via react-markdown + remark-gfm (tables, task lists,
// strike, autolinks) and remark-math + rehype-katex for $/$$ KaTeX math.
// Fenced code blocks go through CodeViewer for syntax highlighting; inline
// code is a styled <code>. Links open in the system browser.
//
// The math pre-pass in mathNormalize normalises LLM-native \(…\)/\[…\]
// delimiters to the $/$$ syntax remark-math understands, gates single-$
// pairs through a classifier to avoid false positives on $5, $PATH, etc.,
// and runs KaTeX-specific normalisations (text-mode escapes, |→\vert).

const STATUS_MARKER_RE = /(?:✅|☑|☒|✔️?|✓|\[[xX ]\])/;
const STATUS_MARKER_GLOBAL_RE = /(?:✅|☑|☒|✔️?|✓|\[[xX ]\])/g;
const BULLET_RE = /^[-*•]\s+\S/;
const DIVIDER_RE = /^[\s\-_=─━—]+$/;

function markdownLinkLabel(children: ReactNode, href: string | undefined, info: GitHubLinkInfo | null) {
  if (!info) return children;
  const childText = Array.isArray(children) ? children.join("") : typeof children === "string" ? children : "";
  return childText === href ? info.compactLabel : children;
}

function GitHubMark() {
  return (
    <svg
      aria-hidden="true"
      fill="none"
      height="13"
      viewBox="0 0 24 24"
      width="13"
    >
      <path
        d="M15 22v-4a4.8 4.8 0 0 0-1-3.5c3.3-.4 6.8-1.6 6.8-7A5.4 5.4 0 0 0 19.4 4 5 5 0 0 0 19.3.5S18.2.1 15 1.8a13.4 13.4 0 0 0-7 0C4.8.1 3.7.5 3.7.5A5 5 0 0 0 3.6 4a5.4 5.4 0 0 0-1.4 3.7c0 5.4 3.5 6.6 6.8 7A4.8 4.8 0 0 0 8 18v4"
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="2"
      />
      <path d="M8 19c-3 .9-3-1.5-4-2" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" />
    </svg>
  );
}

function LinkMark({ kind }: { kind: LinkIconKind }) {
  if (kind === "github") return <GitHubMark />;
  if (kind === "mail") return <Mail aria-hidden="true" size={13} strokeWidth={2} />;
  return <ExternalLink aria-hidden="true" size={13} strokeWidth={2} />;
}

function splitStatusLine(line: string): string[] {
  const parts = (line.match(STATUS_MARKER_GLOBAL_RE) ?? []).length > 1
    ? line.split(/(?=(?:✅|☑|☒|✔️?|✓|\[[xX ]\]))/)
    : [line];
  return parts
    .map((part) => part.replace(/^(?:✅|☑|☒|✔️?|✓|\[[xX ]\]|[-*•])\s*/i, "").trim())
    .filter(Boolean)
    .map((part) => part.replace(/\s{2,}/g, " · "));
}

function looksLikeDiagram(text: string): boolean {
  return /[←→↔]|<{1,2}-{2,}|-{2,}>{1,2}|[-_=─━]{6,}/.test(text);
}

function splitPlainBlock(text: string): { preText: string; statusItems: string[] } {
  const items: string[] = [];
  const preLines: string[] = [];
  const lines = text.split(/\r?\n/);
  const bulletLines = lines.filter((line) => BULLET_RE.test(line.trim())).length;
  const collectBulletLines = bulletLines >= 2 && !looksLikeDiagram(text);
  for (const rawLine of lines) {
    const line = rawLine.trim();
    const marked = STATUS_MARKER_RE.test(line) || (collectBulletLines && BULLET_RE.test(line));
    if (marked) {
      items.push(...splitStatusLine(line));
    } else if (DIVIDER_RE.test(line) && items.length > 0 && !looksLikeDiagram(text)) {
      continue;
    } else {
      preLines.push(rawLine);
    }
  }
  while (preLines.length > 0 && preLines[0].trim() === "") preLines.shift();
  while (preLines.length > 0 && preLines[preLines.length - 1].trim() === "") preLines.pop();
  return { preText: preLines.join("\n"), statusItems: items };
}

function PlainMarkdownBlock({ text }: { text: string }) {
  const { preText, statusItems } = splitPlainBlock(text);
  const asList = statusItems.length >= 2;
  return (
    <div className={`md-plain-block${asList ? " md-plain-block--split" : " md-plain-block--pre"}`}>
      <CodeViewer value={text} maxHeight={360} />
      {asList && preText && (
        <div className="md-plain-block__diagram">
          <CodeViewer value={preText} maxHeight={360} />
        </div>
      )}
      {asList && (
        <div className="md-status-list">
          {statusItems.map((item, index) => (
            <div className="md-status-list__item" key={`${index}-${item}`}>
              <span className="md-status-list__dot" aria-hidden="true" />
              <span className="md-status-list__text">{item}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function createComponents(plainStatusBlocks: boolean): Components {
  return {
    pre: ({ children }) => <>{children}</>,
    code: ({ className, children }) => {
      const text = String(children ?? "");
      const match = /language-([\w-]+)/.exec(className ?? "");
      const lang = match?.[1];
      const isBlock = match !== null || text.includes("\n");
      if (isBlock) {
        const value = text.replace(/\n$/, "");
        if (lang === "mermaid") {
          return (
            <Suspense fallback={<CodeViewer value={value} language="mermaid" maxHeight={360} />}>
              <MermaidDiagram definition={value} />
            </Suspense>
          );
        }
        if (!match && plainStatusBlocks) return <PlainMarkdownBlock text={text.replace(/\n$/, "")} />;
        return <CodeViewer value={value} language={lang} maxHeight={360} />;
      }
      return <code className="md-code">{children}</code>;
    },
    a: ({ href, children }) => {
      const github = parseGitHubLink(href);
      const iconKind = classifyLinkIcon(href);
      const label = markdownLinkLabel(children, href, github);
      const title = github
        ? `${github.owner}/${github.repo} ${github.kind} ${github.value}`
        : undefined;
      return (
        <a
          className={iconKind ? `md-rich-link md-rich-link--${iconKind}` : undefined}
          href={href}
          title={title}
          onClick={(e) => {
            e.preventDefault();
            if (href) openExternal(href);
          }}
          onAuxClick={(e) => {
            e.preventDefault();
            if (href) openExternal(href);
          }}
          onMouseDown={(e) => {
            if (e.button === 1) e.preventDefault();
          }}
        >
          {iconKind && <LinkMark kind={iconKind} />}
          <span>{label}</span>
        </a>
      );
    },
    img: ({ src, alt, title }) => (
      <img
        src={markdownImageSource(src)}
        alt={alt ?? ""}
        title={title}
        loading="lazy"
        referrerPolicy="no-referrer"
      />
    ),
  };
}

const MarkdownRenderer = memo(function MarkdownRenderer({
  text,
  plainStatusBlocks = false,
}: {
  text: string;
  plainStatusBlocks?: boolean;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const mathContent = useMemo(() => normalizeMath(text), [text]);
  const components = useMemo(() => createComponents(plainStatusBlocks), [plainStatusBlocks]);
  return (
    <div className="md" ref={containerRef}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm, remarkMath]}
        rehypePlugins={[rehypeKatex]}
        components={components}
      >
        {mathContent}
      </ReactMarkdown>
    </div>
  );
});

export default MarkdownRenderer;
