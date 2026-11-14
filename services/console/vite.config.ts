import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

// In development the UI runs on 5173 and talks to a gateway on 8090; in the container nginx
// proxies the same paths, so the app never needs to know where the gateway is.
const paths = ["/admin", "/stats", "/health", "/v1", "/mcp"];

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const gateway = env.GATEWAY_URL || "http://127.0.0.1:8090";
  const proxy = Object.fromEntries(paths.map((p) => [p, { target: gateway, changeOrigin: true }]));
  return {
    plugins: [react()],
    server: { port: 5173, proxy },
    build: { outDir: "dist", sourcemap: false },
  };
});
