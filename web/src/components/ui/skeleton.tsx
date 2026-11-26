import * as React from "react";

import { cn } from "@/lib/utils";

const Skeleton = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function Skeleton({ className, ...props }, ref) {
  return <div ref={ref} data-slot="skeleton" className={cn("bg-accent animate-pulse rounded-md", className)} {...props} />;
});

export { Skeleton };
