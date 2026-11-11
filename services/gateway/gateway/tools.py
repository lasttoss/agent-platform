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

SALARY_VND = {"1001": 12_345_000, "2002": 9_876_000}   # demo fixtures, synthetic numbers
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


def load_salary_overrides(values: dict | None) -> None:
    """Salary fixtures are not committed: real amounts live in config/local.json."""
    for uid, amount in (values or {}).items():
        SALARY_VND[str(uid)] = int(amount)


def call(name: str, principal: dict, args: dict) -> dict:
    if name not in TOOLS:
        raise ToolError(f"unknown tool: {name}")
    if name == "salary_self":
        # No user_id argument on purpose: the caller cannot ask for someone else.
        uid = principal["user_id"]
        if uid not in SALARY_VND:
            return {"found": False}
        return {"found": True, "amount_vnd": SALARY_VND[uid], "currency": "VND"}
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
