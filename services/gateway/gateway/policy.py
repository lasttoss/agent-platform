"""Authorization rules. This module is the only place that decides what a caller
may do. It is pure: it reads config, returns decisions, touches no network.
"""
from __future__ import annotations

import json
from pathlib import Path

from .tools import TOOLS


class Denied(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def load_local(path: str | Path) -> dict:
    """Real platform user ids and real salary fixtures live here, never in git.

    The committed policy stays a template with demo users; anything that identifies a
    real person is loaded from `config/local.json`, which is gitignored.
    """
    p = Path(path)
    return json.loads(p.read_text(encoding="utf-8")) if p.exists() else {}


def apply_local(rules: dict, consent: dict, local: dict) -> tuple[dict, dict]:
    for uid, spec in (local.get("users") or {}).items():
        rules.setdefault("users", {})[uid] = spec
    for uid, providers in (local.get("consent") or {}).items():
        consent.setdefault(uid, {}).update(providers)
    return rules, consent


def load_consent(path: str | Path) -> dict:
    p = Path(path)
    return json.loads(p.read_text(encoding="utf-8")) if p.exists() else {}


def consent_allows(principal: dict, tool: str, consent: dict) -> bool:
    """A tool whose provider the caller has not consented to is not available.

    No consent file entry = no grant. This is the same rule as the chat allowlist:
    the tool stays invisible instead of failing later.
    """
    spec = TOOLS.get(tool, {})
    provider = spec.get("provider")
    if provider is None:
        return True
    granted = consent.get(principal["user_id"], {}).get(provider, [])
    need = spec.get("scope")
    return True if need in (None, "") else need in granted


def load_rules(path: str | Path) -> dict:
    rules = json.loads(Path(path).read_text(encoding="utf-8"))
    for field in ("users", "chats", "quota"):
        if field not in rules:
            # Fail closed: a malformed policy must never degrade into "allow".
            raise Denied("policy_invalid", f"policy missing field: {field}")
    return rules


def resolve(rules: dict, platform_user_id: str, chat_type: str) -> dict:
    if not rules.get("allow_unknown_users", False):
        user = rules["users"].get(str(platform_user_id))
        if user is None:
            raise Denied("unknown_user", "caller is not provisioned")
    else:
        user = rules["users"].get(str(platform_user_id), {"name": "guest", "role": "guest", "workspace": "guest"})
    chat = rules["chats"].get(chat_type)
    if chat is None:
        raise Denied("unknown_chat", f"chat type not allowed: {chat_type}")
    return {
        "user_id": str(platform_user_id),
        "name": user["name"],
        "role": user["role"],
        "workspace": user["workspace"],
        "chat_type": chat_type,
        "personal_data": bool(chat["personal_data"]),
    }


def chat_conf(rules: dict, principal: dict) -> dict:
    return rules["chats"][principal["chat_type"]]


def free_chat_enabled(rules: dict, principal: dict) -> bool:
    """May the model answer when no tool matched?

    Default is no. A gateway whose model is not allowed to be a source of facts must not
    fall back to ungrounded chat just because the request missed every tool: that path is
    where a demo starts inventing answers under the owner's name. Opting in is explicit,
    per chat, in the policy file.
    """
    return bool(chat_conf(rules, principal).get("free_chat", False))


def routable_tools(rules: dict, principal: dict) -> list[str]:
    """Tools the gateway may execute for this caller in this chat: model-visible
    tools plus private-only tools (personal ones that must never be posted here)."""
    conf = chat_conf(rules, principal)
    names = list(conf.get("tools", [])) + list(conf.get("private_tools", []))
    out = []
    for name in names:
        spec = TOOLS.get(name)
        if spec is None or spec["destructive"]:
            continue
        out.append(name)
    return out


def visible_tools(rules: dict, principal: dict) -> list[str]:
    """Tools the model is even told about. Filtering happens here, not after a call."""
    allowed = []
    for name in chat_conf(rules, principal).get("tools", []):
        spec = TOOLS.get(name)
        if spec is None or spec["destructive"]:
            continue
        if spec["personal"] and not principal["personal_data"]:
            continue
        allowed.append(name)
    return allowed


def decide(rules: dict, principal: dict, tool: str) -> str:
    """Return "call" or "call_and_deliver_privately"."""
    spec = TOOLS.get(tool)
    if spec is None:
        raise Denied("unknown_tool", f"no such tool: {tool}")
    if spec["destructive"]:
        raise Denied("destructive_blocked", f"{tool} is not allowed from chat")
    conf = chat_conf(rules, principal)
    in_chat = tool in conf.get("tools", [])
    in_private = tool in conf.get("private_tools", [])
    # A private-only slot must hold personal tools only; anything else fails closed.
    if in_private and not spec["personal"]:
        raise Denied("policy_invalid", f"{tool} is private but not personal")
    if not in_chat and not in_private:
        raise Denied("tool_not_in_chat", f"{tool} is not enabled for {principal['chat_type']}")
    if spec["personal"] and not principal["personal_data"]:
        # Personal answer in a group: compute it, never post it here.
        return "call_and_deliver_privately"
    return "call"
