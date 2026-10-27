"""Minimal cloud-side tool stub for the Termio reference gateway.

The cloud agent's tool implementations do not touch the local filesystem; they
forward the call to the gateway, which routes it over the daemon's outbound
WebSocket. Use a stable call_id when retrying: the daemon caches results for
10 minutes and replays them instead of re-executing.

This sample uses only the standard library. Do not put a long-lived token in a
browser or desktop client; the gateway call belongs on the cloud side.
"""

import json
import urllib.request

GATEWAY = "https://gateway.example.com"
DEVICE_ID = "dev_replace_with_generated_id"
TOKEN = "replace-with-cloud-or-device-token"


def call_tool(name, arguments, timeout_ms=30000, call_id=None):
    body = {
        "name": name,
        "arguments": arguments,
        "timeout_ms": timeout_ms,
    }
    if call_id:
        body["call_id"] = call_id
    request = urllib.request.Request(
        f"{GATEWAY}/v1/devices/{DEVICE_ID}/tools/call",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout_ms / 1000 + 5) as response:
        return json.load(response)


def search_files(pattern, path=None, max_results=50, call_id=None):
    arguments = {"pattern": pattern, "max_results": max_results}
    if path:
        arguments["path"] = path
    result = call_tool("search_files", arguments, call_id=call_id)
    if result.get("isError"):
        raise RuntimeError(result["content"][0]["text"])
    return result.get("structuredContent", {})


if __name__ == "__main__":
    print(json.dumps(search_files("*.go", max_results=10), indent=2))
