---
name: smoke
description: Run an end-to-end check of the pilots integration: create a throwaway machine, run a command in it, checkpoint it, restore it, then destroy it after the user approves the cleanup.
disable-model-invocation: true
---

# pilots smoke test

Proves the whole path works: tools, key, create, exec, checkpoint, restore.
Cleanup is the one step that destroys something, so it waits for approval.

1. Run the `status` checks first. Stop and report if the tools are missing or
   the key does not work.
2. `create_machine` with `name` `smoke-<four random letters>` and no other
   arguments. Record the id, name and URL it returns.
3. `exec` on it: `["bash", "-c", "echo hello from $(hostname) && uname -r"]`.
   Expect exit code 0 and output containing `hello from`.
4. `checkpoint` it with comment `smoke`. Record the checkpoint id.
5. `exec`: `["bash", "-c", "echo changed > /home/pilot/smoke.txt"]`, then
   `restore` the checkpoint, then `exec` `["cat", "/home/pilot/smoke.txt"]`.
   Expect a non-zero exit: the file must be gone, because restore is in
   place and discards what came after the checkpoint.
6. Ask the user whether to destroy the machine. Only after a yes, call
   `destroy_machine`. If they say no, report its name so they can find it in
   `pilot machines` or the dashboard.

Report every step with the exact identifiers, and the one step that failed if
any did, in this form:

```text
pilots smoke
- status:     OK
- create:     m-… (smoke-abcd) https://…
- exec:       exit 0
- checkpoint: ck-…
- restore:    OK (file gone after restore)
- destroy:    done | kept (smoke-abcd)
```
