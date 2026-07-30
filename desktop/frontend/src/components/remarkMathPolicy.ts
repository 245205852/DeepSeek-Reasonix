import type { Root, Text } from "mdast";
import type { InlineMath } from "mdast-util-math";
import { visit } from "unist-util-visit";
import { classifyInlineMath } from "./mathClassify";
import { latexNormalizeForKatex } from "./latexNormalize";

type HastLikeNode = {
  type: string;
  value?: string;
  children?: HastLikeNode[];
};

type ParentWithChildren = {
  children: Array<{ type: string; value?: unknown; position?: unknown }>;
};

type VFileLike = {
  value: unknown;
};

function siblingText(parent: ParentWithChildren, index: number, offset: -1 | 1): string {
  const sibling = parent.children[index + offset];
  if (sibling?.type !== "text" || typeof sibling.value !== "string") return "";
  return offset < 0 ? sibling.value.slice(-120) : sibling.value.slice(0, 120);
}

function originalSource(node: InlineMath, file: VFileLike): string {
  const source = String(file.value ?? "");
  const start = node.position?.start.offset;
  const end = node.position?.end.offset;
  if (typeof start === "number" && typeof end === "number") {
    return source.slice(start, end);
  }
  return `$${node.value}$`;
}

function setMathValue(
  node: { value: string; data?: unknown },
  value: string,
): void {
  node.value = value;

  // mdast-util-math caches hast children while parsing. Keep that rendering
  // payload in sync with node.value when a later remark plugin normalises it.
  const data = node.data as { hChildren?: HastLikeNode[] } | undefined;
  const hChildren = data?.hChildren;
  const updateFirstText = (children: HastLikeNode[] | undefined): boolean => {
    if (!children) return false;
    for (const child of children) {
      if (child.type === "text") {
        child.value = value;
        return true;
      }
      if (updateFirstText(child.children)) return true;
    }
    return false;
  };
  updateFirstText(hChildren);
}

/**
 * Semantic policy layered after remark-math has parsed Markdown boundaries.
 * Code spans/fences never become math nodes, so this plugin only decides how
 * already-parsed inline math should render.
 */
export function remarkMathPolicy() {
  return (tree: Root, file: VFileLike) => {
    visit(tree, "inlineMath", (node, index, parent) => {
      if (typeof index !== "number" || !parent) return;

      const typedNode = node as InlineMath;
      const typedParent = parent as ParentWithChildren;
      const decision = classifyInlineMath(typedNode.value, {
        before: siblingText(typedParent, index, -1),
        after: siblingText(typedParent, index, 1),
      });

      if (decision === "math") {
        setMathValue(typedNode, latexNormalizeForKatex(typedNode.value));
        return;
      }

      const value = decision === "currency"
        ? `$${typedNode.value}`
        : originalSource(typedNode, file);
      typedParent.children[index] = { type: "text", value } satisfies Text;
    });

    visit(tree, "math", (node) => {
      setMathValue(node, latexNormalizeForKatex(node.value));
    });
  };
}
