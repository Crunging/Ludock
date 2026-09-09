import { defineConfig } from "vite";

// Serve only built frontend files. The normal development configuration proxies
// API requests to a backend; browser fixtures must never reach that backend.
export default defineConfig({
  preview: {
    host: "127.0.0.1",
    port: Number(process.env.LUDOCK_E2E_PORT || 4179),
    strictPort: true,
  },
});
