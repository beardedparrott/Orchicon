import ReactMarkdown from "react-markdown";
import remarkEmoji from "remark-emoji";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

import { remarkHardBreaks } from "@/lib/remarkHardBreaks";
import { cn } from "@/lib/utils";

const COMPONENTS: Components = {
  h1: ({ className, ...props }) => (
    <h1 className={cn("mb-3 mt-5 text-xl font-bold first:mt-0", className)} {...props} />
  ),
  h2: ({ className, ...props }) => (
    <h2 className={cn("mb-2 mt-4 text-lg font-semibold", className)} {...props} />
  ),
  h3: ({ className, ...props }) => (
    <h3 className={cn("mb-1.5 mt-3 text-base font-medium", className)} {...props} />
  ),
  p: ({ className, ...props }) => (
    <p className={cn("mb-2 last:mb-0", className)} {...props} />
  ),
  ul: ({ className, ...props }) => (
    <ul className={cn("mb-2 list-disc pl-5 last:mb-0", className)} {...props} />
  ),
  ol: ({ className, ...props }) => (
    <ol className={cn("mb-2 list-decimal pl-5 last:mb-0", className)} {...props} />
  ),
  li: ({ className, ...props }) => (
    <li className={cn("mb-0.5", className)} {...props} />
  ),
  code: ({ className, children, ...props }) => {
    const isInline = !className;
    if (isInline) {
      return (
        <code
          className="rounded px-1 py-0.5 font-mono text-xs [background:color-mix(in_srgb,currentColor_12%,transparent)]"
          {...props}
        >
          {children}
        </code>
      );
    }
    return (
      <pre className="mb-2 overflow-x-auto rounded-md p-3 last:mb-0 [background:color-mix(in_srgb,currentColor_10%,transparent)]">
        <code className={cn("font-mono text-xs leading-relaxed", className)} {...props}>
          {children}
        </code>
      </pre>
    );
  },
  pre: ({ children }) => <>{children}</>,
  blockquote: ({ className, ...props }) => (
    <blockquote
      className={cn("mb-2 border-l-2 border-muted-foreground/30 pl-3 italic text-muted-foreground last:mb-0", className)}
      {...props}
    />
  ),
  table: ({ className, ...props }) => (
    <div className="mb-2 overflow-x-auto last:mb-0">
      <table className={cn("w-full text-left text-sm", className)} {...props} />
    </div>
  ),
  th: ({ className, ...props }) => (
    <th className={cn("border-b px-2 py-1 font-medium text-muted-foreground", className)} {...props} />
  ),
  td: ({ className, ...props }) => (
    <td className={cn("border-b px-2 py-1", className)} {...props} />
  ),
  a: ({ className, ...props }) => (
    <a
      className={cn("text-primary underline underline-offset-2 hover:text-primary/80", className)}
      target="_blank"
      rel="noopener noreferrer"
      {...props}
    />
  ),
  hr: ({ className, ...props }) => (
    <hr className={cn("my-3 border-muted-foreground/20", className)} {...props} />
  ),
};

export function Markdown({
  children,
  className,
  preserveBreaks = false,
}: {
  children?: string | null;
  className?: string;
  /**
   * preserveBreaks keeps a newline the author TYPED as a line break, instead of letting CommonMark collapse it
   * to a space.
   *
   * IT IS OPT-IN, AND THAT IS THE WHOLE POINT. CommonMark's soft break is correct for prose the model wrote —
   * that text is machine-wrapped and joining its lines is what lets it reflow to any width. It is wrong for
   * text a person typed by hand, where the line they broke is a line they meant (see remarkHardBreaks.ts). Only
   * the user-authored surfaces pass it, so enabling it here cannot reflow the assistant's answers, the system
   * prompt panel, reasoning, or any stored description that happens to be rendered through this component.
   */
  preserveBreaks?: boolean;
}) {
  if (!children) return null;
  const remarkPlugins = preserveBreaks
    ? [remarkGfm, remarkEmoji, remarkHardBreaks]
    : [remarkGfm, remarkEmoji];
  return (
    <div className={cn("prose-custom text-sm leading-relaxed break-words [overflow-wrap:anywhere]", className)}>
      <ReactMarkdown components={COMPONENTS} remarkPlugins={remarkPlugins}>
        {children}
      </ReactMarkdown>
    </div>
  );
}
