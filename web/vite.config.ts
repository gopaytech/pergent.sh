/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { writeFileSync } from "node:fs";
import { defineConfig, type Plugin } from "vite";

// emptyOutDir removes the .keep that keeps dist/ in git; put it back.
const keepFile = (): Plugin => ({
  name: "keep-file",
  apply: "build",
  closeBundle() {
    writeFileSync(new URL("../internal/web/dist/.keep", import.meta.url), "");
  },
});

// The build goes straight into the Go package that embeds it. The server's CSP
// allows no inline script or style, so nothing here may inline either.
export default defineConfig({
  plugins: [react(), keepFile()],
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    assetsInlineLimit: 0,
  },
  server: {
    // `npm run dev` talks to a pergent-sh serve on :8080.
    proxy: {
      "/api": "http://localhost:8080",
      "/auth": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    globals: false,
  },
});
