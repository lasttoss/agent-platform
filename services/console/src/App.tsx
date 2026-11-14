import { useCallback, useEffect, useState } from "react";
import { api, ApiError, RoleRow, tokenStore, UserRow } from "./api";

type Status = { kind: "idle" | "ok" | "bad"; text: string };

export default function App() {
  const [token, setToken] = useState(tokenStore.get());
  const [hasToken, setHasToken] = useState(!!tokenStore.get());
  const [users, setUsers] = useState<UserRow[]>([]);
  const [roles, setRoles] = useState<RoleRow[]>([]);
  const [generation, setGeneration] = useState<number | null>(null);
  const [status, setStatus] = useState<Status>({ kind: "idle", text: "" });
  const [busy, setBusy] = useState<string | null>(null);

  const load = useCallback(async (message?: string) => {
    if (!tokenStore.get()) {
      setStatus({ kind: "idle", text: "Paste a token to start." });
      return;
    }
    try {
      const r = await api.roles();
      setRoles(r.roles);
      const u = await api.users();
      setUsers(u.users);
      setGeneration(u.generation);
      setStatus(message ? { kind: "ok", text: message } : { kind: "idle", text: "" });
    } catch (e) {
      const err = e as ApiError;
      setStatus({ kind: "bad", text: `Cannot load: ${err.message}` });
      if (err.status === 401) {
        setHasToken(false);
      }
    }
  }, []);

  // Load once on mount if this tab already holds a token; every other load is an explicit act.
  // Relying on the hasToken effect instead meant a second "Load" did nothing, because the flag
  // was already true — a silent no-op is the worst kind of button.
  useEffect(() => {
    if (tokenStore.get()) void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function submitToken(ev: React.FormEvent) {
    ev.preventDefault();
    const next = token.trim();
    if (!next) {
      setStatus({ kind: "bad", text: "Token is empty." });
      return;
    }
    tokenStore.set(next);
    setToken("");
    setHasToken(true);
    await load();
  }

  async function changeRole(user: UserRow, next: string) {
    if (next === user.role) {
      setStatus({ kind: "idle", text: `No change for ${user.subject}.` });
      return;
    }
    setBusy(user.subject);
    try {
      const out = await api.setRole(user.subject, next);
      await load(`${out.subject}: ${out.from} → ${out.to} (generation ${out.generation})`);
    } catch (e) {
      setStatus({ kind: "bad", text: `Refused: ${(e as Error).message}` });
      await load();
    } finally {
      setBusy(null);
    }
  }

  function forget() {
    tokenStore.clear();
    setHasToken(false);
    setUsers([]);
    setRoles([]);
    setGeneration(null);
    setStatus({ kind: "idle", text: "Token forgotten for this tab." });
  }

  return (
    <>
      <header>
        <h1>Agent platform · console</h1>
        <p className="sub">
          Login happens upstream. Paste the token of an account whose role has <code>admin: true</code>.
        </p>
      </header>

      <main>
        <section>
          <div className="sec-head">
            <h2>Session</h2>
            <span className="note">token stays in this tab</span>
          </div>
          <form className="token" onSubmit={submitToken}>
            <input
              type="password"
              placeholder={hasToken ? "token loaded — paste another to replace" : "bearer token"}
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-label="bearer token"
            />
            <button type="submit">Load</button>
            <button type="button" className="ghost" onClick={forget}>
              Forget
            </button>
          </form>
        </section>

        <section>
          <div className="sec-head">
            <h2>People</h2>
            <span className="note">
              generation <span className="gen">{generation ?? "–"}</span>
            </span>
          </div>
          {users.length === 0 ? (
            <p className="empty">{hasToken ? "Nobody provisioned." : "No token loaded."}</p>
          ) : (
            <ul className="users">
              {users.map((u) => (
                <li className="user" key={u.subject}>
                  <div className="who">
                    <span className="name">{u.name || u.subject}</span>
                    <span className="meta">
                      {u.subject} · role from {u.source}
                    </span>
                  </div>
                  <div className="controls">
                    <select
                      value={u.role}
                      onChange={(e) => void changeRole(u, e.target.value)}
                      disabled={busy === u.subject}
                      aria-label={`role for ${u.subject}`}
                    >
                      {roles.map((r) => (
                        <option key={r.role} value={r.role}>
                          {r.role}
                        </option>
                      ))}
                    </select>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section>
          <div className="sec-head">
            <h2>Roles</h2>
            <span className="note">what each role may do</span>
          </div>
          {roles.length === 0 ? (
            <p className="empty">No token loaded.</p>
          ) : (
            <ul className="roles">
              {roles.map((r) => (
                <li className="role" key={r.role}>
                  <div className="role-name">
                    {r.role}
                    {r.admin && <span className="tag">admin</span>}
                  </div>
                  <div className="tools">{r.tools.length ? r.tools.join(", ") : "no tools"}</div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <p className={`status ${status.kind}`} role="status" aria-live="polite">
          {status.text}
        </p>
      </main>
    </>
  );
}
