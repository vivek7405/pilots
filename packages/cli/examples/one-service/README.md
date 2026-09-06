# One service, no configuration

A webjs app with no Dockerfile and no compose file. The platform works out what
it is.

```
pilot deploy
```

That is the whole thing. The directory is tarred and posted to the plan route,
the host recognises the `@webjsdev/*` dependency, generates a Dockerfile with
`PORT=8080` and a health check on `/__webjs/ready`, builds it, creates the
service and waits for the health gate.

The URL is `<name>.<fleet domain>`, derived from the directory name unless you
pass `--app`, and it never changes again: not on a redeploy, not on a suspend,
not when the machine moves to another host.

From an agent, the same thing is one tool call:

```json
{ "name": "deploy", "arguments": { "dir": "/abs/path/to/one-service" } }
```
