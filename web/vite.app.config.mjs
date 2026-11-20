// web/vite.app.config.mjs — the stylesheet build for server-rendered pages.
//
// This is the ONLY vite build left (the SolidJS SPA was deleted with D-WEB-14);
// it uses the already-installed vite + @tailwindcss/vite toolchain, so no extra
// CLI package is required. Output is unhashed `dist/app.css` because templates
// reference /_ui/app.css (Cache-Control: no-cache + ETag), not a
// content-hashed asset name. `public/` still lands in dist/ (vite copies it by
// default), which is where /_ui/concepts/*.gif come from.
import { defineConfig } from "vite";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: false, // dist also carries the island/vendor/SDK bundles
    cssCodeSplit: false,
    rollupOptions: {
      input: "src/app-entry.js",
      output: { assetFileNames: "app.css", entryFileNames: "app-entry.js" },
    },
  },
});
