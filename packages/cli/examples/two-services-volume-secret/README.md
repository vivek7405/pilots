# Two services, a durable volume and two secrets

A webjs app in front of Postgres. Everything that makes this more than one
service is in `compose.yaml`, and nothing in it is a value anyone would mind
committing.

```
pilot secrets set postgres_password
pilot secrets set database_url
pilot deploy
```

The two `pilot secrets set` commands prompt. The value is never an argument, so
it is not in a shell history and it does not pass through a model.

Three things this file demonstrates:

- **`secret://name`** is a reference the deploy resolves. The host seals the
  values with the fleet key before they touch any replicated row.
- **The volume outlives the release.** `pgdata` is attached to the Postgres
  machine and survives every redeploy; anything written into the rootfs does
  not. A volume-backed service runs exactly one replica, because a volume is
  mounted by one machine at a time.
- **`postgres.internal`** is how `web` reaches the database. Services sharing an
  app name resolve each other by name, which is why `DATABASE_URL` points at a
  hostname nothing had to allocate.

`pilot add postgres` writes this compose file for you.
