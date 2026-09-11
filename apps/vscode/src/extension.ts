/**
 * The pilots extension: a machine as a folder you are editing.
 *
 * Seven commands, and the one that matters is Open Machine -- it adds a
 * `pilot://<name>/<path>` workspace folder, after which every ordinary thing
 * the editor does (open, edit, save, search, rename) happens inside the
 * machine. The terminal command opens a real shell there through the CLI, so
 * the editing surface and the shell agree about what is on disk.
 *
 * The token is kept in VS Code's own secret storage, never in a setting: a
 * setting is synced, shown in a settings UI, and readable by every other
 * extension.
 */

import * as vscode from 'vscode'
// The SDK is ESM and this extension is the CommonJS module VS Code's host
// loads, so the client is imported dynamically where it is built and only the
// TYPES are imported statically, with the attribute that resolves them as ESM.
import type { Machine, PilotsClient } from '@pilots/sdk' with { 'resolution-mode': 'import' }

import { PilotsFileSystem } from './filesystem.ts'

const SECRET_KEY = 'pilots.apiKey'
const SCHEME = 'pilot'

let client: PilotsClient | null = null
const fs = new PilotsFileSystem()

export function activate(context: vscode.ExtensionContext): void {
  // Registered FIRST and unconditionally: a workspace saved with a
  // `pilot://` folder is reopened before any token has been read, and a
  // provider that arrived late would have already failed those reads.
  context.subscriptions.push(
    vscode.workspace.registerFileSystemProvider(SCHEME, fs, { isCaseSensitive: true, isReadonly: false }),
  )

  void context.secrets.get(SECRET_KEY).then(async (key) => {
    if (key) client = await connect(key)
    fs.setClient(client)
  })

  context.subscriptions.push(
    vscode.commands.registerCommand('pilots.setToken', async () => {
      const key = await vscode.window.showInputBox({
        prompt: 'Your pilots API token (pilot login prints one, or the Tokens page mints one)',
        password: true,
        ignoreFocusOut: true,
        validateInput: (value) => (value.trim() ? null : 'A token is required'),
      })
      if (!key) return
      await context.secrets.store(SECRET_KEY, key.trim())
      client = await connect(key.trim())
      fs.setClient(client)
      try {
        const who = await client.whoami()
        vscode.window.showInformationMessage(`pilots: signed in${who.org_id ? ` as ${who.org_id}` : ''}`)
      } catch (err) {
        vscode.window.showErrorMessage(`pilots: that token did not work: ${message(err)}`)
      }
    }),

    vscode.commands.registerCommand('pilots.open', async () => {
      const machine = await pickMachine('Open which machine?')
      if (!machine) return
      const home = vscode.workspace.getConfiguration('pilots').get<string>('home') || '/home/pilot'
      const path = await vscode.window.showInputBox({
        prompt: `Path inside ${machine.name}`,
        value: home,
        ignoreFocusOut: true,
      })
      if (!path) return

      const uri = vscode.Uri.parse(`${SCHEME}://${machine.name}${path.startsWith('/') ? path : `/${path}`}`)
      const folders = vscode.workspace.workspaceFolders ?? []
      // Appended rather than replacing: a person opening a machine beside
      // their own repository is the point, and replacing the workspace would
      // close what they were working on.
      vscode.workspace.updateWorkspaceFolders(folders.length, 0, { uri, name: machine.name })
    }),

    vscode.commands.registerCommand('pilots.create', async () => {
      if (!(await requireClient())) return
      const name = await vscode.window.showInputBox({
        prompt: 'A name for the machine. Its URL is derived from it and never changes.',
        ignoreFocusOut: true,
      })
      if (name === undefined) return
      await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Notification, title: 'pilots: creating' },
        async () => {
          try {
            const machine = await client!.machines.create(name ? { name } : {})
            const open = await vscode.window.showInformationMessage(
              `pilots: created ${machine.name} at ${machine.url}`,
              'Open',
            )
            if (open === 'Open') await vscode.commands.executeCommand('pilots.open')
          } catch (err) {
            vscode.window.showErrorMessage(`pilots: ${message(err)}`)
          }
        },
      )
    }),

    vscode.commands.registerCommand('pilots.terminal', async () => {
      const machine = await pickMachine('Open a terminal in which machine?')
      if (!machine) return
      // Through the CLI rather than a websocket of our own: `pilot console`
      // already handles the PTY, the resize protocol and the reconnect, and a
      // second implementation of a terminal is a second one to get wrong.
      const terminal = vscode.window.createTerminal({
        name: `pilots: ${machine.name}`,
        shellPath: 'pilot',
        shellArgs: ['console', machine.name],
      })
      terminal.show()
    }),

    vscode.commands.registerCommand('pilots.destroy', async () => {
      const machine = await pickMachine('Destroy which machine?')
      if (!machine) return
      const confirmed = await vscode.window.showWarningMessage(
        `Destroy ${machine.name}? Its disk, snapshots and URL go with it.`,
        { modal: true },
        'Destroy',
      )
      if (confirmed !== 'Destroy') return
      try {
        await client!.machines.destroy(machine.id)
        // The folder would otherwise stay in the workspace pointing at
        // nothing, and every read from it would look like a bug.
        const folder = (vscode.workspace.workspaceFolders ?? []).findIndex(
          (f) => f.uri.scheme === SCHEME && f.uri.authority === machine.name,
        )
        if (folder >= 0) vscode.workspace.updateWorkspaceFolders(folder, 1)
        vscode.window.showInformationMessage(`pilots: destroyed ${machine.name}`)
      } catch (err) {
        vscode.window.showErrorMessage(`pilots: ${message(err)}`)
      }
    }),

    vscode.commands.registerCommand('pilots.refresh', () => {
      fs.invalidate()
    }),

    vscode.commands.registerCommand('pilots.download', async (uri?: vscode.Uri) => {
      const source = uri ?? vscode.window.activeTextEditor?.document.uri
      if (!source || source.scheme !== SCHEME) {
        vscode.window.showErrorMessage('pilots: that is not a file on a machine')
        return
      }
      const target = await vscode.window.showSaveDialog({
        defaultUri: vscode.Uri.file(source.path.split('/').pop() ?? 'download'),
      })
      if (!target) return
      try {
        await vscode.workspace.fs.writeFile(target, await vscode.workspace.fs.readFile(source))
        vscode.window.showInformationMessage(`pilots: saved ${target.fsPath}`)
      } catch (err) {
        vscode.window.showErrorMessage(`pilots: ${message(err)}`)
      }
    }),
  )
}

export function deactivate(): void {
  client = null
  fs.setClient(null)
}

/**
 * The client, built from the stored token and the configured fleet.
 *
 * `import()` rather than a top-level import: the SDK is ESM-only and this
 * file is loaded by VS Code's CommonJS host, where a static import of an ESM
 * package is a `require` of it and throws at activation.
 */
async function connect(key: string): Promise<PilotsClient> {
  const { PilotsClient: Client } = await import('@pilots/sdk')
  const configured = vscode.workspace.getConfiguration('pilots').get<string>('apiUrl')
  return new Client(key, configured ? { baseURL: configured } : {})
}

async function requireClient(): Promise<boolean> {
  if (client) return true
  const set = await vscode.window.showErrorMessage('pilots: no API token yet', 'Set Token')
  if (set === 'Set Token') await vscode.commands.executeCommand('pilots.setToken')
  return false
}

async function pickMachine(placeHolder: string): Promise<Machine | undefined> {
  if (!(await requireClient())) return undefined
  let machines: Machine[]
  try {
    machines = await client!.machines.list()
  } catch (err) {
    vscode.window.showErrorMessage(`pilots: ${message(err)}`)
    return undefined
  }
  const live = machines.filter((m) => m.state !== 'destroyed')
  if (live.length === 0) {
    const create = await vscode.window.showInformationMessage('pilots: no machines yet', 'Create Machine')
    if (create === 'Create Machine') await vscode.commands.executeCommand('pilots.create')
    return undefined
  }
  const picked = await vscode.window.showQuickPick(
    live.map((m) => ({ label: m.name, description: m.state, detail: m.url, machine: m })),
    { placeHolder },
  )
  return picked?.machine
}

/** An SDK error's own words: it carries `next`, which is the useful half. */
function message(err: unknown): string {
  const e = err as { message?: string; next?: string }
  return e?.next ? `${e.message} (${e.next})` : (e?.message ?? String(err))
}
