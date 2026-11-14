import { expect, test } from "@playwright/test";

// The admin token is a demo token from config/dsh-tokens.json, mapped to user 1001, whose role in
// policy.json has admin: true. Nothing secret lives here.
const ADMIN = process.env.CONSOLE_TOKEN || "dsh-demo-owner-token";
const SUBJECT = "2002";

test("a demotion takes effect on the next MCP call", async ({ page, request }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: /console/i })).toBeVisible();

  await page.getByLabel("bearer token").fill(ADMIN);
  await page.getByRole("button", { name: "Load" }).click();

  const row = page.getByRole("listitem").filter({ hasText: SUBJECT });
  await expect(row).toBeVisible();
  const generation = await page.locator(".gen").textContent();

  // The MCP call goes through nginx, exactly as a tool client would.
  const call = (tool: string) =>
    request.post("/mcp", {
      headers: { Authorization: `Bearer dsh-demo-dev-token`, "Content-Type": "application/json" },
      data: { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: tool, arguments: { q: "e2e" } } },
    });

  const before = await call("wiki_search");
  expect(await before.text()).toContain("result");

  await row.getByRole("combobox").selectOption("guest");
  await expect(page.getByRole("status")).toContainText(`${SUBJECT}: dev → guest`);
  await expect(page.locator(".gen")).not.toHaveText(generation ?? "");

  const after = await call("wiki_search");
  expect(await after.text()).toContain("tool_not_in_role");

  // Leave the platform as it was found, so the suite can run twice in a row.
  await row.getByRole("combobox").selectOption("dev");
  await expect(page.getByRole("status")).toContainText(`${SUBJECT}: guest → dev`);
  const restored = await call("wiki_search");
  expect(await restored.text()).toContain("result");
});

test("invalid arguments are refused before the service is called", async ({ request }) => {
  const res = await request.post("/mcp", {
    headers: { Authorization: "Bearer dsh-demo-owner-token", "Content-Type": "application/json" },
    data: { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "wiki_search", arguments: { q: 42 } } },
  });
  expect(await res.text()).toContain("invalid arguments");
});
