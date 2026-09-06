# Volumes

## What this covers

Data that survives a redeploy: declaring a volume, what a volume-backed service can and cannot do, and the Postgres case. Not covered: the rest of the compose file (compose.md).

## The command to copy

In a compose file:

    services:
      postgres:
        image: postgres:17
        volumes:
          - pgdata:/var/lib/postgresql/data
    volumes:
      pgdata:
        driver_opts:
          size: 10G

Then `pilot deploy`. MCP: `volumes` lists what exists.

## What happens

1. The plan creates the volume before the service that mounts it.
2. The volume is attached to one machine and survives every redeploy of that service. The rootfs does not: it is replaced by the next release.
3. A volume-backed service runs exactly ONE replica, because a volume is mounted by one machine at a time.

## The rules

| Rule | Why |
| --- | --- |
| one replica per volume-backed service | one machine mounts it at a time; two would corrupt it |
| a volume is declared in a compose file | it is part of the app's shape, not a thing to create by hand |
| a redeploy keeps the data | the volume outlives the rootfs |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `volume_in_use`, 409 | something else holds it | `next` names what: destroy that machine, or detach it from that service |
| `bad_request` about replicas | a volume-backed service was asked for more than one replica | drop the replicas, or drop the volume |

## Do not

- Do not write data you need into the rootfs. The next release replaces it.
- Do not scale a volume-backed service. It is refused, and for a good reason.
- Do not create a volume by hand for a service a compose file already describes.
