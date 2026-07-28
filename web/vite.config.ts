import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The built output is embedded in the Go binary and served from /assets. Hashed file names
// are kept: they make the immutable cache headers honest, because a new build produces new
// URLs rather than reusing an old one.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/webassets/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
    target: "es2022",
  },
  server: {
    proxy: { "/api": "http://127.0.0.1:8080", "/s": "http://127.0.0.1:8080" },
  },
});
