import * as React from "react";

import { cn } from "@/lib/utils";

const Card = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function Card({ className, ...props }, ref) {
  return (
    <div ref={ref}
      data-slot="card"
      className={cn("bg-card text-card-foreground flex flex-col gap-4 rounded-xl border py-4 shadow-xs", className)}
      {...props}
    />
  );
});

const CardHeader = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function CardHeader({ className, ...props }, ref) {
  return (
    <div ref={ref}
      data-slot="card-header"
      className={cn(
        "@container/card-header grid auto-rows-min grid-rows-[auto_auto] items-start gap-1 px-4 has-data-[slot=card-action]:grid-cols-[1fr_auto] [.border-b]:pb-4",
        className,
      )}
      {...props}
    />
  );
});

const CardTitle = React.forwardRef<HTMLHeadingElement, React.ComponentPropsWithoutRef<"h2">>(function CardTitle({ className, ...props }, ref) {
  return <h2 ref={ref} data-slot="card-title" className={cn("text-[15px] leading-tight font-semibold", className)} {...props} />;
});

const CardDescription = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function CardDescription({ className, ...props }, ref) {
  return <div ref={ref} data-slot="card-description" className={cn("text-muted-foreground text-sm", className)} {...props} />;
});

const CardAction = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function CardAction({ className, ...props }, ref) {
  return (
    <div ref={ref}
      data-slot="card-action"
      className={cn("col-start-2 row-span-2 row-start-1 self-start justify-self-end", className)}
      {...props}
    />
  );
});

const CardContent = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function CardContent({ className, ...props }, ref) {
  return <div ref={ref} data-slot="card-content" className={cn("px-4", className)} {...props} />;
});

const CardFooter = React.forwardRef<HTMLDivElement, React.ComponentPropsWithoutRef<"div">>(function CardFooter({ className, ...props }, ref) {
  return <div ref={ref} data-slot="card-footer" className={cn("flex items-center px-4 [.border-t]:pt-4", className)} {...props} />;
});

export { Card, CardAction, CardContent, CardDescription, CardFooter, CardHeader, CardTitle };
