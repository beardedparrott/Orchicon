import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

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
          className="rounded bg-muted px-1 py-0.5 font-mono text-xs"
          {...props}
        >
          {children}
        </code>
      );
    }
    return (
      <pre className="mb-2 overflow-x-auto rounded-md bg-muted p-3 last:mb-0">
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

export function Markdown({ children, className }: { children?: string | null; className?: string }) {
  if (!children) return null;
  return (
    <div className={cn("prose-custom text-sm leading-relaxed", className)}>
      <ReactMarkdown components={COMPONENTS} remarkPlugins={[remarkGfm]}>
        {children}
      </ReactMarkdown>
    </div>
  );
}
