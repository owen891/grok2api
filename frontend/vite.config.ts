import path from "node:path";
import { readFileSync } from "node:fs";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const appVersion = readFileSync(path.resolve(__dirname, "../VERSION"), "utf8").trim();

export default defineConfig({
  plugins: [react(), tailwindcss()],
  define: {
    __GROK2API_DEV_API_TARGET__: JSON.stringify(process.env.VITE_DEV_API_TARGET ?? ""),
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": process.env.VITE_DEV_API_TARGET ?? "http://127.0.0.1:8000",
      "/v1": process.env.VITE_DEV_API_TARGET ?? "http://127.0.0.1:8000",
      "/healthz": process.env.VITE_DEV_API_TARGET ?? "http://127.0.0.1:8000",
      "/readyz": process.env.VITE_DEV_API_TARGET ?? "http://127.0.0.1:8000",
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
  },
});
