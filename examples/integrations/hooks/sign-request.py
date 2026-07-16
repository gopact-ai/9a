#!/usr/bin/env python3
import hashlib
import hmac
import json
import os
import sys

request = json.load(sys.stdin)
secret = os.environ["ORDER_SIGNING_SECRET"].encode()
payload = json.dumps(request.get("query", {}), sort_keys=True).encode()
request.setdefault("headers", {})["X-Signature"] = hmac.new(
    secret, payload, hashlib.sha256
).hexdigest()
json.dump(request, sys.stdout, separators=(",", ":"))
