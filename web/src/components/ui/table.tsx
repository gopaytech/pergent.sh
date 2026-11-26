import * as React from "react";

import { cn } from "@/lib/utils";

const Table = React.forwardRef<HTMLTableElement, React.ComponentPropsWithoutRef<"table">>(function Table({ className, ...props }, ref) {
  return (
    <div data-slot="table-container" className="relative w-full overflow-x-auto">
      <table ref={ref} data-slot="table" className={cn("w-full caption-bottom text-sm", className)} {...props} />
    </div>
  );
});

const TableHeader = React.forwardRef<HTMLTableSectionElement, React.ComponentPropsWithoutRef<"thead">>(function TableHeader({ className, ...props }, ref) {
  return <thead ref={ref} data-slot="table-header" className={cn("[&_tr]:border-b", className)} {...props} />;
});

const TableBody = React.forwardRef<HTMLTableSectionElement, React.ComponentPropsWithoutRef<"tbody">>(function TableBody({ className, ...props }, ref) {
  return <tbody ref={ref} data-slot="table-body" className={cn("[&_tr:last-child]:border-0", className)} {...props} />;
});

const TableFooter = React.forwardRef<HTMLTableSectionElement, React.ComponentPropsWithoutRef<"tfoot">>(function TableFooter({ className, ...props }, ref) {
  return (
    <tfoot ref={ref}
      data-slot="table-footer"
      className={cn("bg-muted/50 border-t font-medium [&>tr]:last:border-b-0", className)}
      {...props}
    />
  );
});

const TableRow = React.forwardRef<HTMLTableRowElement, React.ComponentPropsWithoutRef<"tr">>(function TableRow({ className, ...props }, ref) {
  return (
    <tr ref={ref}
      data-slot="table-row"
      className={cn("hover:bg-muted/50 data-[state=selected]:bg-muted border-b transition-colors", className)}
      {...props}
    />
  );
});

const TableHead = React.forwardRef<HTMLTableCellElement, React.ComponentPropsWithoutRef<"th">>(function TableHead({ className, ...props }, ref) {
  return (
    <th ref={ref}
      data-slot="table-head"
      className={cn(
        "text-muted-foreground h-10 px-3 text-left align-middle font-medium whitespace-nowrap [&:has([role=checkbox])]:pr-0",
        className,
      )}
      {...props}
    />
  );
});

const TableCell = React.forwardRef<HTMLTableCellElement, React.ComponentPropsWithoutRef<"td">>(function TableCell({ className, ...props }, ref) {
  return (
    <td ref={ref}
      data-slot="table-cell"
      className={cn("p-3 align-middle [&:has([role=checkbox])]:pr-0", className)}
      {...props}
    />
  );
});

const TableCaption = React.forwardRef<HTMLTableCaptionElement, React.ComponentPropsWithoutRef<"caption">>(function TableCaption({ className, ...props }, ref) {
  return <caption ref={ref} data-slot="table-caption" className={cn("text-muted-foreground mt-4 text-sm", className)} {...props} />;
});

export { Table, TableBody, TableCaption, TableCell, TableFooter, TableHead, TableHeader, TableRow };
