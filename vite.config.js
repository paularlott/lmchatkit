import { defineConfig } from "vite";
import { resolve } from "path";

// chat.js is a CLASSIC script, not an ES module: it mutates `window`
// (processMarkdown, escapeHtml) and registers an `alpine:init` listener so
// hosts can load it with a plain deferred <script> tag, no module system
// required. vite's rollup pipeline is module-oriented by default, so we pin
// output.format to "iife" — this wraps the side effects in an IIFE (keeping
// them, plus the explicit window.xxx assignments) without injecting
// import/export syntax that would break the non-module <script> load.
//
// Output is a stable web/dist/chat.js (no content hash) because go:embed
// references the literal path; the handler computes its own ETag from the
// bytes.
export default defineConfig({
  root: "web",
  publicDir: false,
  build: {
    outDir: "dist",
    emptyOutDir: true,
    manifest: false,
    minify: "esbuild",
    rollupOptions: {
      input: {
        chat: resolve(__dirname, "web/src/chat.js"),
      },
      output: {
        entryFileNames: "[name].js",
        chunkFileNames: "[name].js",
        assetFileNames: "[name].[ext]",
        format: "iife",
      },
    },
  },
});
