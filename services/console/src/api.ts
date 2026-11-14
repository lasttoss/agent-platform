// The console talks to four endpoints and nothing else. The token never leaves the tab, and no
// answer is cached here: a stale permission on screen is worse than a loading state.

export type RoleRow = { role: string; tools: string[]; admin: boolean };
export type UserRow = {
  subject: string;
  name?: string;
  role: string;
  in_policy: boolean;
  source: "policy" | "console";
};

const TOKEN_KEY = "agent-platform.token";

export const tokenStore = {
  get: () => sessionStorage.getItem(TOKEN_KEY) ?? "",
  set: (t: string) => sessionStorage.setItem(TOKEN_KEY, t.trim()),
  clear: () => sessionStorage.removeItem(TOKEN_KEY),
};

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      Authorization: `Bearer ${tokenStore.get()}`,
      "Content-Type": "application/json",
      ...(init?.headers ?? {}),
    },
  });
  const text = await res.text();
  let body: any = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    /* not json */
  }
  if (!res.ok) {
    throw new ApiError(res.status, body?.error ?? body?.message ?? `HTTP ${res.status}`);
  }
  return body as T;
}

export type Me = {
  user: string;
  name?: string;
  role: string;
  workspace?: string;
  models: string[];
  admin: boolean;
  token_masked: string;
  token_id: string;
};
export type TokenRow = { id: string; user: string; masked: string };
export type NewToken = { token: string; masked: string; user: string; id?: string; note?: string };

export const api = {
  roles: () => call<{ roles: RoleRow[] }>("/admin/roles"),
  users: () => call<{ users: UserRow[]; generation: number }>("/admin/users"),
  setRole: (subject: string, role: string) =>
    call<{ subject: string; from: string; to: string; generation: number }>(
      `/admin/users/${encodeURIComponent(subject)}/role`,
      { method: "POST", body: JSON.stringify({ role }) },
    ),
  health: () => call<{ ok: boolean }>("/health"),
  me: () => call<Me>("/me"),
  rotateMyToken: () => call<NewToken>("/me/token/rotate", { method: "POST" }),
  tokens: () => call<{ tokens: TokenRow[] }>("/admin/tokens"),
  createToken: (user: string) =>
    call<NewToken>("/admin/tokens", { method: "POST", body: JSON.stringify({ user }) }),
  revokeToken: (id: string) =>
    call<{ revoked: string; user: string }>(`/admin/tokens/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
};
