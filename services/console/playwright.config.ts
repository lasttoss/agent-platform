import { defineConfig, devices } from "@playwright/test";

// End-to-end against the running platform, not a mock: the point is to prove nginx, the gateway,
// the role store and the MCP front agree with each other. Start the stack first (compose) or set
// CONSOLE_URL to wherever it is.
export default defineConfig({
  testDir: "e2e",
  timeout: 30_000,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: process.env.CONSOLE_URL || "http://127.0.0.1:8088",
    trace: "retain-on-failure",
    ...devices["Pixel 5"],
  },
});
