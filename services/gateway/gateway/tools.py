"""Demo tool registry. Every tool declares whether it returns personal data.

The gateway never lets the model pick a tool that is not in the caller's allowed
list, and it never passes a "whose data" argument: the subject is always the
authenticated principal.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request

# Demo fixtures. The digits are a counting pattern (12-345-000, 9-876-000), not a market
# rate: an example that could pass for real data is a liability, not an illustration.
SALARY = {"1001": {"amount": 12_345_000, "currency": "VND"},
          "2002": {"amount": 9_876_000, "currency": "VND"}}
DOCS = [
    ("runbook-restart", "How to restart the game server without dropping sessions"),
    ("policy-data", "What counts as personal data in our systems"),
    ("oncall", "On-call rota and escalation path"),
]

TOOLS: dict[str, dict] = {
    "salary_self": {"personal": True, "destructive": False,
                    "description": "Return the salary record of the caller."},
    "docs_search": {"personal": False, "destructive": False,
                    "description": "Search internal documents."},
    "drive_search": {"personal": True, "destructive": False, "provider": "google_drive",
                     "scope": "drive.readonly",
                     "description": "Search the caller's own Drive."},
    "jira_search": {"personal": False, "destructive": False, "provider": "jira",
                    "scope": "read:jira-work",
                    "description": "Search Jira issues visible to the caller."},
    "mail_list": {"personal": True, "destructive": False, "provider": "imap",
                  "scope": "mail.read",
                  "description": "List recent mail for the caller."},
    "status": {"personal": False, "destructive": False,
               "description": "Report service health."},
    "ocr_extract": {"personal": True, "destructive": False, "provider": "local_ocr",
                    "description": "Extract text from a PDF or image through the OCR tool service."},
    "deploy_prod": {"personal": False, "destructive": True,
                    "description": "Deploy to production. Never exposed to chat."},
}


class ToolError(RuntimeError):
    pass


def _call_ocr(path: str) -> dict:
    """Talk to the OCR worker over HTTP. The gateway never imports its code."""
    url = os.environ.get("OCR_SERVICE_URL", "http://127.0.0.1:8099").rstrip("/") + "/v1/ocr"
    body = json.dumps({"path": path}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        try:
            return json.loads(exc.read().decode())
        except Exception:
            return {"error": "ocr_bad_response", "detail": str(exc)[:200]}
    except (urllib.error.URLError, TimeoutError) as exc:
        return {"error": "ocr_unavailable", "detail": str(exc)[:200]}


# Argument schemas. Kept here, next to the implementation, so a tool cannot exist without
# a stated interface — and so the model can only be offered parameters we actually parse.
PARAMS = {
    "salary_self": {"type": "object", "properties": {}},
    "docs_search": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
    "status": {"type": "object", "properties": {}},
    "ocr_extract": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]},
    "drive_search": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
    "jira_search": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
    "mail_list": {"type": "object", "properties": {}},
}


def schema(name: str) -> dict:
    """OpenAI-shaped tool definition. This is the ONLY thing the model is ever told about,
    so the visible list and the offered list can never disagree."""
    spec = TOOLS[name]
    params = PARAMS.get(name) or {"type": "object", "properties": {}}
    fn = {"name": name, "description": spec["description"], "parameters": params}
    return {"type": "function", "function": fn}


def load_salary_overrides(values: dict | None) -> None:
    """Real amounts live in config/local.json; accepts either 40000000 or
    {"amount": 40000000, "currency": "VND"} so the file stays readable."""
    for uid, value in (values or {}).items():
        if isinstance(value, dict):
            SALARY[str(uid)] = {"amount": int(value["amount"]),
                                "currency": value.get("currency", "VND")}
        else:
            SALARY[str(uid)] = {"amount": int(value), "currency": "VND"}


def call(name: str, principal: dict, args: dict) -> dict:
    if name not in TOOLS:
        raise ToolError(f"unknown tool: {name}")
    if name == "salary_self":
        # No user_id argument on purpose: the caller cannot ask for someone else.
        uid = principal["user_id"]
        if uid not in SALARY:
            return {"found": False}
        record = SALARY[uid]
        return {"found": True, "amount": record["amount"], "currency": record["currency"]}
    if name == "docs_search":
        q = (args.get("q") or "").lower()
        hits = [{"id": i, "title": t} for i, t in DOCS if q in i or q in t.lower()]
        return {"query": q, "hits": hits[:3]}
    if name == "status":
        return {"services": {"game": "ok", "db": "ok", "redis": "ok"}}
    if name == "drive_search":
        return {"hits": [], "note": "mock provider: no Drive credentials configured"}
    if name == "jira_search":
        return {"hits": [], "note": "mock provider: no Jira credentials configured"}
    if name == "mail_list":
        return {"messages": [], "note": "mock provider: no mailbox configured"}
    if name == "ocr_extract":
        return _call_ocr(args.get("path", ""))
    raise ToolError(f"tool not callable from chat: {name}")
