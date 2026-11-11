"""Telegram long-polling adapter for the gateway.

The adapter does one thing: turn a platform update into an authenticated request.
It never decides who may see what — the gateway does that, and the gateway's answer
already says `delivered: chat | dm`. The adapter's only security job is to refuse
updates from anyone the gateway does not know, and to keep the chat id it uses for
a DM equal to the sender's own id (never a chat id taken from message text).

Run:  BOT_TOKEN=... GATEWAY_URL=http://127.0.0.1:8088 python3 adapters/telegram_poll.py
"""
from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

API = os.environ.get("TELEGRAM_API_BASE", "https://api.telegram.org")
GATEWAY = os.environ.get("GATEWAY_URL", "http://127.0.0.1:8088")
OFFSET_FILE = Path(os.environ.get("OFFSET_FILE", "state/telegram_offset.json"))
POLL_TIMEOUT = int(os.environ.get("POLL_TIMEOUT", "25"))


def _post(url: str, payload: dict, timeout: int = 30) -> dict:
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def gateway_call(payload: dict, timeout: int = 60) -> tuple[int, dict]:
    """POST to the gateway and return (status, body) — including for 4xx, which the
    gateway uses for 'unknown user' and 'quota exceeded'. Those are answers, not crashes."""
    req = urllib.request.Request(f"{GATEWAY}/v1/respond",
                                data=json.dumps(payload).encode(),
                                headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")


def api_call(token: str, method: str, payload: dict, timeout: int = 30) -> dict:
    return _post(f"{API}/bot{token}/{method}", payload, timeout)


def send(token: str, chat_id: int, text: str, retries: int = 2) -> bool:
    for attempt in range(retries + 1):
        try:
            api_call(token, "sendMessage", {"chat_id": chat_id, "text": text,
                                            "disable_web_page_preview": True})
            return True
        except urllib.error.HTTPError as e:
            body = e.read().decode(errors="replace")
            # 429: respect retry_after instead of hammering the platform
            try:
                wait = json.loads(body)["parameters"]["retry_after"]
            except Exception:
                wait = 2 ** attempt
            print(f"  send failed {e.code}: {body[:120]} (retry in {wait}s)", file=sys.stderr)
            time.sleep(min(wait, 10))
        except Exception as e:                     # network blip
            print(f"  send failed: {e}", file=sys.stderr)
            time.sleep(2 ** attempt)
    return False


def determine_chat(update: dict) -> tuple[int, str, str]:
    """Return (user_id, chat_type, text). chat_type is the gateway's vocabulary."""
    msg = update.get("message") or update.get("edited_message") or {}
    sender = msg.get("from") or {}
    chat = msg.get("chat") or {}
    user_id = int(sender.get("id") or 0)
    kind = chat.get("type", "private")
    if kind == "private":
        chat_type = "dm"
    elif kind in ("group", "supergroup"):
        chat_type = "group_internal"     # refinement below: public channels are not groups
    else:
        chat_type = "group_public"
    return user_id, chat_type, (msg.get("text") or "").strip()


def load_offset() -> int:
    try:
        return int(json.loads(OFFSET_FILE.read_text())["offset"])
    except Exception:
        return 0


def save_offset(offset: int) -> None:
    OFFSET_FILE.parent.mkdir(parents=True, exist_ok=True)
    OFFSET_FILE.write_text(json.dumps({"offset": offset}))


def handle_update(token: str, update: dict) -> dict:
    user_id, chat_type, text = determine_chat(update)
    chat_id = int(((update.get("message") or {}).get("chat") or {}).get("id") or 0)
    if not text:
        return {"skipped": "no_text"}

    started = time.time()
    try:
        status, body = gateway_call({"platform_user_id": user_id,
                                     "chat_type": chat_type, "text": text})
    except Exception as e:                      # gateway down: say so, do not retry forever
        send(token, chat_id, "Hệ thống đang lỗi, thử lại sau ít phút.")
        return {"user": user_id, "status": "gateway_unreachable", "error": str(e)}

    if status == 403:
        send(token, chat_id, "Bạn chưa được cấp quyền dùng trợ lý này.")
        return {"user": user_id, "status": 403}
    if status != 200:
        send(token, chat_id, "Hệ thống đang lỗi, thử lại sau ít phút.")
        return {"user": user_id, "status": status, "ms": int((time.time() - started) * 1000), "body": body}

    # chat reply goes where the message came from, but a private result NEVER goes to a group
    if body.get("chat_reply"):
        send(token, chat_id, body["chat_reply"])
    dm = body.get("dm")
    if isinstance(dm, dict):                   # gateway contract: {"text": ..., "reason": ...}
        dm = dm.get("text")
    if dm:
        send(token, user_id, dm)               # DM target is the sender id, not the chat id
    return {"user": user_id, "chat": chat_type, "delivered": body.get("delivered"),
            "ms": int((time.time() - started) * 1000),
            "cache": body.get("cache"), "tools": body.get("tools_visible")}


def main() -> int:
    token = os.environ.get("BOT_TOKEN", "").strip()
    if not token:
        print("BOT_TOKEN is not set", file=sys.stderr)
        return 2
    try:
        me = api_call(token, "getMe", {})["result"]
    except urllib.error.HTTPError as e:
        print(f"getMe failed ({e.code}) — check the token", file=sys.stderr)
        return 2
    print(f"adapter up as @{me.get('username')} → {GATEWAY}", file=sys.stderr)

    offset = load_offset()
    while True:
        try:
            data = api_call(token, "getUpdates",
                            {"offset": offset, "timeout": POLL_TIMEOUT, "allowed_updates": ["message"]},
                            timeout=POLL_TIMEOUT + 10)
        except urllib.error.HTTPError as e:
            if e.code == 409:
                print("409 Conflict: another process is polling this bot token. Stop it first "
                      "(pi-telegram bridge or a second adapter).", file=sys.stderr)
                return 3
            print(f"getUpdates {e.code}", file=sys.stderr)
            time.sleep(3)
            continue
        except Exception as e:
            print(f"getUpdates error: {e}", file=sys.stderr)
            time.sleep(3)
            continue

        for update in data.get("result", []):
            offset = update["update_id"] + 1
            save_offset(offset)
            print(json.dumps(handle_update(token, update), ensure_ascii=False), file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
