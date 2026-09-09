import { defineConfig, type ProxyOptions } from "vite";
import react from "@vitejs/plugin-react";
import type { ClientRequest, IncomingMessage } from "node:http";

const instance = process.env.LUDOCK_DEV_INSTANCE;
const backendProxy: ProxyOptions = {
  target: process.env.LUDOCK_DEV_API_ORIGIN || "http://localhost:3001",
  ...(instance ? { headers: { "X-Ludock-Dev-Instance": instance } } : {}),
  configure(proxy) {
    if (!instance) return;
    // Browser cookies are shared by host, not port. Forward only this
    // checkout's session so another local backend never receives its peers'.
    const filterCookies = (
      request: ClientRequest,
      incoming: IncomingMessage,
    ) => {
      const cookie = incoming.headers.cookie
        ?.split(";")
        .map((part) => part.trim())
        .find((part) => part.startsWith(`ludock_session_${instance}=`));
      if (cookie) request.setHeader("Cookie", cookie);
      else request.removeHeader("Cookie");
    };
    proxy.on("proxyReq", filterCookies);
    proxy.on("proxyReqWs", filterCookies);
  },
};

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    strictPort: true,
    proxy: {
      "/api": backendProxy,
      "/ws": {
        ...backendProxy,
        ws: true,
      },
    },
  },
});
