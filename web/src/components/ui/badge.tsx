import { cva, type VariantProps } from "class-variance-authority";
import { Slot } from "radix-ui";
import * as React from "react";

import { cn } from "@/lib/utils";

// The tones are the review states: approved (ok), blocking (bad), blocked by
// CI or failed (warn), running (run), and the rest (grey).
const badgeVariants = cva(
  "inline-flex items-center justify-center rounded-full border px-2 py-0.5 text-xs font-medium w-fit whitespace-nowrap shrink-0 [&>svg]:size-3 gap-1 [&>svg]:pointer-events-none focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px] overflow-hidden",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary text-primary-foreground",
        secondary: "border-transparent bg-secondary text-secondary-foreground",
        destructive: "border-transparent bg-destructive text-white",
        outline: "text-foreground",
        ok: "border-transparent bg-ok-soft text-ok",
        bad: "border-transparent bg-bad-soft text-bad",
        warn: "border-transparent bg-warn-soft text-warn",
        run: "border-transparent bg-run-soft text-run",
        grey: "border-transparent bg-grey-soft text-grey",
        draft: "border-dashed border-grey/40 bg-card text-grey",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

const Badge = React.forwardRef<HTMLSpanElement, React.ComponentPropsWithoutRef<"span"> & VariantProps<typeof badgeVariants> & { asChild?: boolean }>(function Badge({
  className,
  variant,
  asChild = false,
  ...props
}, ref) {
  const Comp = asChild ? Slot.Root : "span";
  return <Comp ref={ref} data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
});

export { Badge, badgeVariants };
