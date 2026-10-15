import { Children, cloneElement, isValidElement } from "react";
import type { ReactElement, ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkEmoji from "remark-emoji";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

import { SearchHighlight } from "@/components/chat/SearchHighlight";
import { remarkHardBreaks } from "@/lib/remarkHardBreaks";
import { cn } from "@/lib/utils";

// markChildren wraps every TEXT RUN inside a block element in SearchHighlight, so an in-transcript search
// can mark the hits where they actually are.
//
// WHY IT RECURSES rather than marking only direct string children: react-markdown hands a paragraph
// ["plain ", <strong>["bold"]</strong>, " tail"] as children, and a match inside the emphasis is exactly
// as findable as one outside it. Marking only the top level would leave hits inside bold/link/code
// unmarked and make the highlight look patchy for no reason the operator could see.
//
// IT CLONES HOST ELEMENTS ONLY IN SO FAR AS IT MUST — `{...el.props, children: …}` — so an element's own
// attributes, className and handler identity are untouched. Nothing here runs when no search is active:
// componentsFor returns the plain COMPONENTS object outright, so the default render path cannot regress.
function markChildren(children: ReactNode, query: string): ReactNode {
  return Children.map(children, (child) => {
    if (typeof child === "string") {
      return <SearchHighlight text={child} query={query} />;
    }
    if (isValidElement(child)) {
      const el = child as ReactElement<{ children?: ReactNode }>;
      if (el.props && "children" in el.props) {
        return cloneElement(el, {
          ...el.props,
          children: markChildren(el.props.children, query),
        } as Partial<{ children?: ReactNode }>);
      }
    }
    return child;
  });
}

/** componentsFor returns the renderer set, with text runs marked when a search query is active. */
function componentsFor(query: string): Components {
  const q = query.trim();
  if (q === "") return COMPONENTS;
  const mark = (children: ReactNode) => markChildren(children, q);
  return {
    ...COMPONENTS,
    // Every element that can HOLD prose. Inline spans (strong/em/a/code) are handled by the recursion
    // above, so only the containers need an entry.
    h1: ({ className, children, ...props }) => (
      <h1 className={cn("mb-3 mt-5 text-xl font-bold first:mt-0", className)} {...props}>{mark(children)}</h1>
    ),
    h2: ({ className, children, ...props }) => (
      <h2 className={cn("mb-2 mt-4 text-lg font-semibold", className)} {...props}>{mark(children)}</h2>
    ),
    h3: ({ className, children, ...props }) => (
      <h3 className={cn("mb-1.5 mt-3 text-base font-medium", className)} {...props}>{mark(children)}</h3>
    ),
    p: ({ className, children, ...props }) => (
      <p className={cn("mb-2 last:mb-0", className)} {...props}>{mark(children)}</p>
    ),
    li: ({ className, children, ...props }) => (
      <li className={cn("mb-0.5", className)} {...props}>{mark(children)}</li>
    ),
    th: ({ className, children, ...props }) => (
      <th className={cn("border-b px-2 py-1 font-medium text-muted-foreground", className)} {...props}>{mark(children)}</th>
    ),
    td: ({ className, children, ...props }) => (
      <td className={cn("border-b px-2 py-1", className)} {...props}>{mark(children)}</td>
    ),
    blockquote: ({ className, children, ...props }) => (
      <blockquote
        className={cn("mb-2 border-l-2 border-muted-foreground/30 pl-3 italic text-muted-foreground last:mb-0", className)}
        {...props}
      >{mark(children)}</blockquote>
    ),
  };
}

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

// NOTE ON `prose-custom`, which used to be on the wrapper div: it was DEAD. Introduced with this
// component and never defined — not in src/index.css (checked across the whole tree, and absent
// from the built stylesheet too), and `@tailwindcss/typography`, whose `prose` modifiers the name
// imitates, is not a dependency. It styled nothing. It also was not needed: the wrapper sets the
// text utilities it actually wants (text-sm / leading-relaxed / the wrapping rule) and every
// element is styled explicitly through COMPONENTS below, so dropping the class changes no pixel.
export function Markdown({
  children,
  className,
  preserveBreaks = false,
  highlight = "",
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
  /**
   * highlight is the ACTIVE in-transcript search query. Every occurrence inside this text is wrapped in
   * <mark>, so the operator can see WHERE the match is rather than only that the message contains one.
   * Empty (the default) renders exactly as before — see componentsFor.
   */
  highlight?: string;
}) {
  if (!children) return null;
  const remarkPlugins = preserveBreaks
    ? [remarkGfm, remarkEmoji, remarkHardBreaks]
    : [remarkGfm, remarkEmoji];
  return (
    <div className={cn("text-sm leading-relaxed break-words [overflow-wrap:anywhere]", className)}>
      <ReactMarkdown components={componentsFor(highlight)} remarkPlugins={remarkPlugins}>
        {children}
      </ReactMarkdown>
    </div>
  );
}
