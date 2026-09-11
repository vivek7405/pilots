---
name: status
description: Check whether the pilots MCP server is reachable and the key works, then list the machines and services it can see, without creating or changing anything.
disable-model-invocation: true
---

# pilots status

A read-only health check. Nothing here creates, mutates, restores or destroys.

1. Confirm the pilots MCP tools are available (`status`, `list_machines`,
   `list_services`). If they are missing, the plugin's server is not loaded:
   tell the user to check `/plugin`, run `/reload-plugins`, and if the
   server is listed but failing, to run `/mcp`. Do not install a CLI or
   register a second server.
2. Call `status`. A 401 means the key is missing or wrong: the plugin reads
   `PILOT_API_KEY` (and `PILOT_API_URL` for a self-hosted fleet) from the
   environment, or the user can complete the browser login in `/mcp` when the
   fleet has one. Retry once after they fix it.
3. Call `list_machines` and `list_services`.
4. Report in this compact form. An empty list is a successful result.

```text
pilots status
- MCP tools: available | missing
- Key: OK | needs login | unknown
- Fleet: <api url>, <n> hosts
- Machines: <n> (name, state, url)
- Services: <n> (name, url)
- Next: one concrete action, or "nothing to do"
```
