/**
 * A machine's filesystem, as a VS Code `FileSystemProvider`.
 *
 * A `pilot://<machine>/<path>` URI becomes a workspace folder, and every read
 * and write is a command in the guest over the SDK's buffered exec. There is
 * no agent to install and no port to open: the same exec route an agent uses
 * carries the bytes, base64-encoded so binaries, CRLF and unicode survive a
 * shell.
 *
 * This file is the editor half only. The commands themselves and the parsing
 * of what comes back live in `guest.ts`, which imports no editor API and is
 * therefore testable without an extension host.
 *
 * The provider is deliberately thin. VS Code asks for `stat` constantly, so
 * the one thing it keeps is a short-lived stat cache; everything else goes to
 * the machine, because a filesystem the editor caches is a filesystem that
 * disagrees with the terminal running beside it.
 */

// Types only, resolved as ESM from this CommonJS module: see extension.ts.
import type { PilotsClient } from '@pilots/sdk' with { 'resolution-mode': 'import' }
import * as vscode from 'vscode'

import { classify, cmd, parseListing, parseStat } from './guest.ts'
import type { Kind } from './guest.ts'

/** How long a stat is trusted. Long enough for one render, short enough to be wrong rarely. */
const STAT_TTL_MS = 2_000

export class PilotsFileSystem implements vscode.FileSystemProvider {
  private readonly emitter = new vscode.EventEmitter<vscode.FileChangeEvent[]>()
  readonly onDidChangeFile = this.emitter.event

  private client: PilotsClient | null = null
  /** name -> machine id, so the authority in a URI is resolved once. */
  private readonly ids = new Map<string, string>()
  private readonly stats = new Map<string, { at: number; stat: vscode.FileStat }>()

  setClient(client: PilotsClient | null): void {
    this.client = client
    this.ids.clear()
    this.stats.clear()
  }

  /** Drops every cached stat, which is what the Refresh command is. */
  invalidate(): void {
    this.stats.clear()
    this.emitter.fire([])
  }

  watch(): vscode.Disposable {
    // Nothing to watch: the guest pushes no events, and polling every open
    // file would be a command per file per interval. The editor still sees a
    // write it made, because the write invalidates that stat itself.
    return new vscode.Disposable(() => {})
  }

  // -- the exec plumbing -------------------------------------------------------

  private async machineFor(uri: vscode.Uri): Promise<string> {
    if (!this.client) {
      throw vscode.FileSystemError.Unavailable('no pilots API token: run "pilots: Set API Token"')
    }
    const name = uri.authority
    let id = this.ids.get(name)
    if (!id) {
      const machines = await this.client.machines.list()
      const found = machines.find((m) => m.name === name || m.id === name)
      if (!found) throw vscode.FileSystemError.FileNotFound(uri)
      id = found.id
      this.ids.set(name, id)
    }
    return id
  }

  private async run(uri: vscode.Uri, command: string): Promise<{ stdout: string; stderr: string; code: number }> {
    const id = await this.machineFor(uri)
    const res = await this.client!.machines.exec(id, { cmd: command })
    return { stdout: res.stdout, stderr: res.stderr, code: res.exit_code }
  }

  private async runOrThrow(uri: vscode.Uri, command: string): Promise<string> {
    const res = await this.run(uri, command)
    if (res.code !== 0) throw asFileSystemError(uri, res.stderr)
    return res.stdout
  }

  private path(uri: vscode.Uri): string {
    return uri.path || '/'
  }

  // -- the provider ------------------------------------------------------------

  async stat(uri: vscode.Uri): Promise<vscode.FileStat> {
    const key = uri.toString()
    const cached = this.stats.get(key)
    if (cached && Date.now() - cached.at < STAT_TTL_MS) return cached.stat

    const res = await this.run(uri, cmd.stat(this.path(uri)))
    const parsed = res.code === 0 ? parseStat(res.stdout) : null
    if (!parsed) throw vscode.FileSystemError.FileNotFound(uri)

    const stat: vscode.FileStat = {
      type: asFileType(parsed.kind),
      size: parsed.size,
      mtime: parsed.mtime,
      ctime: parsed.ctime,
    }
    this.stats.set(key, { at: Date.now(), stat })
    return stat
  }

  async readDirectory(uri: vscode.Uri): Promise<[string, vscode.FileType][]> {
    const out = await this.runOrThrow(uri, cmd.list(this.path(uri)))
    return parseListing(out).map(([name, kind]) => [name, asFileType(kind)])
  }

  async readFile(uri: vscode.Uri): Promise<Uint8Array> {
    const res = await this.run(uri, cmd.read(this.path(uri)))
    if (res.code !== 0) throw asFileSystemError(uri, res.stderr)
    return Uint8Array.from(Buffer.from(res.stdout.trim(), 'base64'))
  }

  async writeFile(
    uri: vscode.Uri,
    content: Uint8Array,
    options: { create: boolean; overwrite: boolean },
  ): Promise<void> {
    const path = this.path(uri)
    if (!options.create || !options.overwrite) {
      const exists = (await this.run(uri, cmd.exists(path))).code === 0
      if (exists && !options.overwrite) throw vscode.FileSystemError.FileExists(uri)
      if (!exists && !options.create) throw vscode.FileSystemError.FileNotFound(uri)
    }
    await this.runOrThrow(uri, cmd.write(path, Buffer.from(content).toString('base64')))
    this.stats.delete(uri.toString())
    this.emitter.fire([{ type: vscode.FileChangeType.Changed, uri }])
  }

  async createDirectory(uri: vscode.Uri): Promise<void> {
    await this.runOrThrow(uri, cmd.mkdir(this.path(uri)))
    this.emitter.fire([{ type: vscode.FileChangeType.Created, uri }])
  }

  async delete(uri: vscode.Uri, options: { recursive: boolean }): Promise<void> {
    await this.runOrThrow(uri, cmd.remove(this.path(uri), options.recursive))
    this.stats.delete(uri.toString())
    this.emitter.fire([{ type: vscode.FileChangeType.Deleted, uri }])
  }

  async rename(from: vscode.Uri, to: vscode.Uri, options: { overwrite: boolean }): Promise<void> {
    if (from.authority !== to.authority) {
      // Moving between machines would be a copy through the editor, which is
      // not what the caller asked for and would silently succeed at half of it.
      throw vscode.FileSystemError.Unavailable('cannot move a file between machines')
    }
    await this.runOrThrow(from, cmd.rename(this.path(from), this.path(to), options.overwrite))
    this.stats.delete(from.toString())
    this.stats.delete(to.toString())
    this.emitter.fire([
      { type: vscode.FileChangeType.Deleted, uri: from },
      { type: vscode.FileChangeType.Created, uri: to },
    ])
  }

  async copy(from: vscode.Uri, to: vscode.Uri, options: { overwrite: boolean }): Promise<void> {
    if (from.authority !== to.authority) throw vscode.FileSystemError.Unavailable('cannot copy between machines')
    await this.runOrThrow(from, cmd.copy(this.path(from), this.path(to), options.overwrite))
    this.emitter.fire([{ type: vscode.FileChangeType.Created, uri: to }])
  }
}

function asFileType(kind: Kind): vscode.FileType {
  switch (kind) {
    case 'directory':
      return vscode.FileType.Directory
    case 'symlink':
      return vscode.FileType.SymbolicLink
    case 'file':
      return vscode.FileType.File
    default:
      return vscode.FileType.Unknown
  }
}

/** The guest's own words, as the error the editor knows how to show. */
function asFileSystemError(uri: vscode.Uri, stderr: string): vscode.FileSystemError {
  switch (classify(stderr)) {
    case 'no-permission':
      return vscode.FileSystemError.NoPermissions(uri)
    case 'not-found':
      return vscode.FileSystemError.FileNotFound(uri)
    case 'exists':
      return vscode.FileSystemError.FileExists(uri)
    case 'not-a-directory':
      return vscode.FileSystemError.FileNotADirectory(uri)
    case 'is-a-directory':
      return vscode.FileSystemError.FileIsADirectory(uri)
    default:
      return vscode.FileSystemError.Unavailable(stderr.trim() || 'the machine refused the operation')
  }
}
