/**
 * A tool service in TypeScript, to prove the boundary is the protocol and not the language.
 *
 * This file is the whole service: ~80 lines, no dependencies, no build step. Node 22 can run
 * TypeScript directly (`--experimental-strip-types`), which is why the example needs no npm
 * install — a demo that requires a dependency tree is a demo nobody runs.
 *
 * The gateway changes: none. It learns about `wiki_search` by asking `tools/list`, exactly as it
 * does for the Go demo service and the two Python services.
 */
import { createServer } from "node:http";

type Json = Record<string, unknown>;

const PAGES = [
  { id: "runbook-oncall", title: "On-call runbook: who to page and when" },
  { id: "adr-cache", title: "ADR 7: three cache layers and their prices" },
  { id: "policy-consent", title: "What a consent grant actually permits" },
];

const TOOLS = [
  {
    name: "wiki_search",
    description: "Search the team wiki. Read-only, no personal data.",
    inputSchema: {
      type: "object",
      properties: { q: { type: "string", description: "Query text." } },
      required: ["q"],
    },
  },
];

function call(name: string, args: Json, caller: string): Json {
  if (name !== "wiki_search") throw new Error(`unknown tool: ${name}`);
  const q = String(args.q ?? "").toLowerCase().trim();
  const hits = PAGES.filter((p) => q === "" || p.title.toLowerCase().includes(q))
    .slice(0, 3)
    .map((p) => ({ id: p.id, title: p.title }));
  // The caller id came from a header, not from the tool arguments: a model must not be able to
  // name whose data it is asking about, in any language.
  return { caller, query: q, hits };
}

function reply(id: unknown, result: Json): Json {
  return { jsonrpc: "2.0", id: id ?? null, result };
}

function handle(body: Json): Json | null {
  const method = String(body.method ?? "");
  const id = body.id ?? null;
  if (method === "initialize") {
    return reply(id, {
      protocolVersion: "2024-11-05",
      serverInfo: { name: "wiki-ts", version: "1.0.0" },
      capabilities: { tools: {} },
    });
  }
  if (method === "notifications/initialized" || method === "ping") return reply(id, {});
  if (method === "tools/list") return reply(id, { tools: TOOLS });
  if (method === "tools/call") {
    const params = (body.params ?? {}) as Json;
    return reply(id, { __call: params }); // handled below, where the header is available
  }
  return { jsonrpc: "2.0", id, error: { code: -32601, message: `method not found: ${method}` } };
}

const port = Number(process.env.PORT ?? 8095);
const server = createServer((req, res) => {
  if (req.method !== "POST" || (req.url ?? "").replace(/\/$/, "") !== "/mcp") {
    res.writeHead(404, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: "not_found" }));
    return;
  }
  let raw = "";
  req.on("data", (chunk) => (raw += chunk));
  req.on("end", () => {
    const send = (payload: Json, code = 200) => {
      res.writeHead(code, { "content-type": "application/json" });
      res.end(JSON.stringify(payload));
    };
    let body: Json;
    try {
      body = JSON.parse(raw || "{}");
    } catch {
      return send({ jsonrpc: "2.0", id: null, error: { code: -32700, message: "parse error" } });
    }
    const out = handle(body);
    if (!out) return send({ jsonrpc: "2.0", id: null, error: { code: -32600, message: "bad request" } });

    // tools/call needs the request, so it is finished here rather than inside handle().
    const pending = (out.result ?? {}) as Json;
    if (pending.__call) {
      const params = pending.__call as { name?: string; arguments?: Json };
      try {
        const result = call(String(params.name), params.arguments ?? {}, req.headers["x-caller-id"] as string ?? "");
        return send(reply(out.id, {
          isError: false,
          content: [{ type: "text", text: JSON.stringify(result) }],
        }));
      } catch (err) {
        return send(reply(out.id, {
          isError: true,
          content: [{ type: "text", text: String(err instanceof Error ? err.message : err) }],
        }));
      }
    }
    send(out);
  });
});

server.listen(port, "127.0.0.1", () => {
  console.log(`wiki-ts MCP service on http://127.0.0.1:${port}/mcp`);
});
