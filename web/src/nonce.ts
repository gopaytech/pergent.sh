import { setNonce } from "get-nonce";

// applyCspNonce hands the page's style nonce to the Radix parts (Dialog,
// DropdownMenu) that add a <style> tag to stop the page scrolling under
// them. The server's CSP allows a style tag only with this nonce; it puts
// a fresh one in <meta name="csp-nonce"> of every index.html it serves.
// Under `npm run dev` there is no CSP and no meta, and nothing is set.
//
// The browser hides the nonce attribute once the page has loaded; the
// element's nonce property still reads it.
export function applyCspNonce(doc: Document = document) {
  const meta = doc.querySelector<HTMLMetaElement>('meta[name="csp-nonce"]');
  const nonce = meta?.nonce || meta?.getAttribute("nonce") || "";
  if (nonce) setNonce(nonce);
  return nonce;
}
