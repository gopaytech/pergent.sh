import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn joins class names and lets a later Tailwind class win over an earlier
// one of the same kind (a page's px-2 over a component's px-4).
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
