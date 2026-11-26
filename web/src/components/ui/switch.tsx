import * as React from "react";

import { cn } from "@/lib/utils";

// Switch is shadcn's switch drawn on a native checkbox with role="switch",
// so it keeps form behaviour and the tests' checkbox events.
const Switch = React.forwardRef<HTMLInputElement, Omit<React.ComponentPropsWithoutRef<"input">, "type">>(function Switch({ className, ...props }, ref) {
  return (
    <input ref={ref}
      type="checkbox"
      role="switch"
      data-slot="switch"
      className={cn(
        "peer relative inline-flex h-[1.15rem] w-8 shrink-0 cursor-pointer appearance-none items-center rounded-full border border-transparent bg-input shadow-xs transition-all outline-none checked:bg-primary focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50",
        "before:pointer-events-none before:block before:size-4 before:rounded-full before:bg-card before:shadow-sm before:transition-transform checked:before:translate-x-[calc(100%-2px)]",
        className,
      )}
      {...props}
    />
  );
});

export { Switch };
