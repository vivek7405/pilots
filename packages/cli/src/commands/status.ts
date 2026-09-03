/**
 * `pilot status`: the fleet as the host that answered sees it.
 *
 * Read from that host's local replica, which is the point: there is no
 * coordinator to ask, so any host can answer and the answer survives the loss
 * of any other one.
 */

import { Command } from 'commander'
import type { Machine, MachineState } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { isJSONMode, printJSON, printTable } from '../output.ts'

export function createStatusCommand(): Command {
  return new Command('status')
    .description('hosts in the fleet and machines by state')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const [hosts, machines] = await Promise.all([client.hosts.list(), client.machines.list()])
      const byState = countByState(machines)

      if (isJSONMode()) {
        printJSON({ hosts, machines_by_state: byState, machines_total: machines.length })
        return
      }
      printTable([
        ['HOST', 'ALIVE', 'CPU FREE', 'MEM FREE MIB'],
        ...hosts.map((h) => [h.id, String(h.alive), String(h.cpu_free), String(h.mem_free_mib)]),
      ])
      process.stdout.write('\n')
      printTable([
        ['STATE', 'MACHINES'],
        ...Object.entries(byState).map(([state, n]) => [state, String(n)]),
        ['total', String(machines.length)],
      ])
    })
}

export function countByState(machines: Machine[]): Record<string, number> {
  const out: Record<string, number> = {}
  // Every state is present with a zero rather than absent: "creating: 0" and
  // "no creating key" read the same to a person and differently to a script.
  for (const state of ['creating', 'running', 'suspended', 'stopped', 'error'] satisfies MachineState[]) {
    out[state] = 0
  }
  for (const machine of machines) out[machine.state] = (out[machine.state] ?? 0) + 1
  return out
}
