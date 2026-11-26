import { ChevronDownIcon } from "lucide-react";
import * as React from "react";

import { cn } from "@/lib/utils";

// NativeSelect is the browser's own select with shadcn's look: it keeps the
// keyboard, the phone picker and form behaviour, and needs no portal.
const NativeSelect = React.forwardRef<HTMLSelectElement, React.ComponentPropsWithoutRef<"select">>(function NativeSelect({ className, ...props }, ref) {
  return (
    <div data-slot="native-select-wrapper" className={cn("relative w-full has-[select:disabled]:opacity-50", className)}>
      <select ref={ref}
        data-slot="native-select"
        className={cn(
          "border-input bg-card bg-none h-9 w-full min-w-0 appearance-none rounded-md border py-1 pr-8 pl-3 text-sm shadow-xs transition-[color,box-shadow] outline-none disabled:pointer-events-none disabled:cursor-not-allowed",
          "focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px]",
          "aria-invalid:ring-destructive/20 aria-invalid:border-destructive",
        )}
        {...props}
      />
      <ChevronDownIcon
        className="text-muted-foreground pointer-events-none absolute top-1/2 right-2.5 size-4 -translate-y-1/2 select-none"
        aria-hidden="true"
      />
    </div>
  );
});

export { NativeSelect };
