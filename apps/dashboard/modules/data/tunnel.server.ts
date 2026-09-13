/**
 * A localhost address that is really a machine on the fleet.
 *
 * # Why a listener and not the Duplex directly
 *
 * The SDK's `machines.tcp` hands back a Duplex, and three of the four drivers
 * would take it. `mongodb` would not: it opens its own sockets through its
 * topology layer and takes a host and a port, never a stream. Rather than keep
 * one driver on a different path -- the path that then rots, because nobody
 * exercises it -- every driver dials 127.0.0.1 and this bridges.
 *
 * The listener binds to a port the KERNEL picks on the loopback interface. That
 * is what keeps it private: an address nothing published, on an interface
 * nothing outside this host can reach.
 *
 * # Why it is per request
 *
 * A pool of tunnels would be a pool of open connections into customer
 * databases, held across requests, keyed by something that has to stay correct.
 * One request, one tunnel, closed in a `finally`. A query costs a connection
 * setup; a leak costs a database.
 */

import { createServer } from 'node:net';
import type { Server, Socket } from 'node:net';
import type { Duplex } from 'node:stream';

import { fleetAs } from '#modules/fleet/client.server.ts';

export interface Tunnel {
  /** Always 127.0.0.1. Named so a caller cannot hardcode the wrong thing. */
  host: string;
  port: number;
  /** Closes the listener and every connection bridged through it. */
  close(): Promise<void>;
}

/**
 * Opens a loopback listener bridged to `port` inside `machineId`.
 *
 * Every accepted connection opens its own tunnel, because a driver may open
 * several: `pg` opens one, `mongodb` opens one per topology member it thinks it
 * has found.
 */
export async function openTunnel(org: string, machineId: string, port: number): Promise<Tunnel> {
  const client = fleetAs(org);
  const live = new Set<Socket | Duplex>();

  const server: Server = createServer((socket: Socket) => {
    live.add(socket);
    socket.on('close', () => live.delete(socket));
    client.machines
      .tcp(machineId, port)
      .then((remote) => {
        live.add(remote);
        remote.on('close', () => live.delete(remote));
        // Piped BOTH ways before anything is read, so a driver that speaks
        // first is not racing the second pipe being attached.
        socket.pipe(remote);
        remote.pipe(socket);
        // A failure on either side ends the other. Left alone, a driver waits
        // for a reply from a tunnel that is already gone, which surfaces as a
        // query that never returns rather than as an error.
        const drop = () => {
          socket.destroy();
          remote.destroy();
        };
        socket.on('error', drop);
        remote.on('error', drop);
        remote.on('end', () => socket.end());
        socket.on('end', () => remote.end());
      })
      .catch(() => socket.destroy());
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    server.close();
    throw new Error('the tunnel listener did not bind to a port');
  }

  return {
    host: '127.0.0.1',
    port: address.port,
    close(): Promise<void> {
      for (const conn of live) conn.destroy();
      live.clear();
      return new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

/**
 * Runs `fn` against a tunnel and closes it however `fn` ends.
 *
 * The shape exists so no caller can forget the close. A tunnel left open is an
 * open connection into somebody's database that nothing will ever reap.
 */
export async function withTunnel<T>(
  org: string,
  machineId: string,
  port: number,
  fn: (tunnel: Tunnel) => Promise<T>,
): Promise<T> {
  const tunnel = await openTunnel(org, machineId, port);
  try {
    return await fn(tunnel);
  } finally {
    await tunnel.close();
  }
}
