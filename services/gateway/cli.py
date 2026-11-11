#!/usr/bin/env python3
"""Ask the gateway from a terminal.

    python3 cli.py --user 1001 --chat dm "lương tháng này của tôi bao nhiêu"
"""
from __future__ import annotations

import argparse
import json
import urllib.request

p = argparse.ArgumentParser()
p.add_argument("text")
p.add_argument("--user", default="1001")
p.add_argument("--chat", default="dm", choices=["dm", "group_internal", "group_public"])
p.add_argument("--url", default="http://127.0.0.1:8088/v1/respond")
a = p.parse_args()

body = json.dumps({"platform_user_id": a.user, "chat_type": a.chat, "text": a.text}).encode()
req = urllib.request.Request(a.url, data=body, headers={"Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req, timeout=30) as r:
        print(json.dumps(json.loads(r.read().decode()), ensure_ascii=False, indent=2))
except urllib.error.HTTPError as e:
    print(e.code, e.read().decode())
