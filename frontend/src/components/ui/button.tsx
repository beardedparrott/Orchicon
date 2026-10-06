import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

// shadcn/ui Button primitive. The base UI primitive that mirrors the
// domain model per docs/10_Frontend_Architecture.md §5.1.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default:
          "bg-primary text-primary-foreground shadow-sm hover:bg-primary/90",
        destructive:
          "bg-destructive text-destructive-foreground shadow-sm hover:bg-destructive/90",
        outline:
          "border border-[hsla(var(--glass-panel-border)/var(--glass-panel-border-a))] bg-white/5 backdrop-blur-sm shadow-sm hover:bg-accent hover:text-accent-foreground",
        secondary:
          "bg-secondary text-secondary-foreground shadow-sm hover:bg-secondary/80",
        ghost: "hover:bg-accent hover:text-accent-foreground",
        link: "text-primary underline-offset-4 hover:underline",
      },
      size: {
        default: "h-9 px-4 py-2",
        sm: "h-8 rounded-md px-3 text-xs",
        lg: "h-10 rounded-md px-8",
        icon: "h-9 w-9",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

// A BUTTON IS NOT A SUBMIT BUTTON UNLESS IT SAYS SO.
//
// HTML's default for a <button> with no `type` is "submit", so a `<Button>`
// rendered inside somebody else's <form> — which is how every reusable
// component here is used (the worker routes mount MCPServersPanel, FileBrowser
// and ModelPicker inside the draft-version form) — submitted that form on
// click. The reported defect: adding an MCP server from the Registry catalog on
// a worker page ran the panel's handler AND the draft form's onSubmit, so the
// version was saved, edit mode closed, and the server never appeared.
//
// Defaulting to "button" makes the whole codebase safe by construction, and an
// intended submit stays a one-word opt-in (`<Button type="submit">`, which is
// how every form here already declares its submit control). This matches the
// convention MUI/Chakra follow for the same reason, and is the reason
// components/FileBrowser.tsx and components/FileInputButton.tsx had each already
// spelled `type="button"` out by hand.
//
// `asChild` passes props to whatever it wraps (already an <a> or a custom
// element), so there the type is left exactly as the caller gave it.
const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, asChild = false, type, ...props }, ref) => {
    const Comp = asChild ? Slot : "button";
    return (
      <Comp
        className={cn(buttonVariants({ variant, size, className }))}
        ref={ref}
        {...(asChild ? {} : { type: type ?? "button" })}
        {...props}
      />
    );
  }
);
Button.displayName = "Button";

export { Button, buttonVariants };
