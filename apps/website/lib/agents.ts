/**
 * The integration menu, in one place.
 *
 * The same table the CLI carries (`agents/harnesses.json`, embedded into the
 * `pilot` binary), so a harness that is installable is documented and a
 * harness that is documented is installable. A test compares the two names
 * lists and fails when they drift, which is the only thing that stops a page
 * from advertising a command that does not exist.
 */

/** A coding agent that can load the pilots MCP server. */
export interface Harness {
  /** What `pilot mcp install <name>` takes. */
  name: string;
  /** What a person calls it. */
  title: string;
  /** The one line that says how it is set up, beyond the install command. */
  note: string;
}

/**
 * Ordered by how many people use them rather than alphabetically, because a
 * reader is scanning for their own editor and stops at the first hit.
 */
export const HARNESSES: Harness[] = [
  { name: 'claude-code', title: 'Claude Code', note: 'Or install the plugin, which adds the skill, two slash commands and a guard.' },
  { name: 'codex', title: 'Codex', note: 'Writes the entry into config.toml and leaves every other line alone.' },
  { name: 'cursor', title: 'Cursor', note: 'User-wide, or per repository with --project.' },
  { name: 'vscode', title: 'VS Code (Copilot)', note: 'For Copilot. The pilots extension is separate and does more.' },
  { name: 'opencode', title: 'OpenCode', note: 'Reads opencode.json, so the entry can live with the repository.' },
  { name: 'windsurf', title: 'Windsurf', note: 'One user-wide file, no per-repository form.' },
  { name: 'gemini', title: 'Gemini CLI', note: 'Settings live under .gemini, either scope.' },
  { name: 'zed', title: 'Zed', note: 'Runs the server on stdio, which is where its context servers live.' },
  { name: 'claude-desktop', title: 'Claude Desktop', note: 'Stdio, so the pilot binary has to be on the PATH it starts with.' },
  { name: 'generic', title: 'Anything else', note: 'Writes an mcp.json you can paste into a client this list does not name.' },
];

/** A typed client, and how it is installed. */
export interface Sdk {
  language: string;
  install: string;
  /** What is true about this one and not the others. */
  note: string;
}

export const SDKS: Sdk[] = [
  { language: 'TypeScript', install: 'npm i @pilots/sdk', note: 'Zero dependencies. Also ships the TanStack AI sandbox provider.' },
  { language: 'Python', install: 'pip install pilots-sdk', note: 'Two dependencies, and the framework adapters live here.' },
  { language: 'Go', install: 'go get github.com/vivek7405/pilots/sdks/go', note: 'One dependency, the same websocket library hostd speaks.' },
  { language: 'Elixir', install: '{:pilots, "~> 0.1"}', note: 'One dependency. The HTTP calls go through OTP\'s own httpc.' },
];

/** A framework that can run its tool calls on a machine. */
export interface Adapter {
  framework: string;
  install: string;
  note: string;
}

export const ADAPTERS: Adapter[] = [
  {
    framework: 'Google ADK',
    install: "pip install 'pilots-sdk[adk]'",
    note: 'Seven tools, including a restore that refuses to run without a confirmation.',
  },
  {
    framework: 'OpenAI Agents SDK',
    install: "pip install 'pilots-sdk[openai-agents]'",
    note: 'A sandbox session per run, or one named machine that outlives them.',
  },
  {
    framework: 'Claude Managed Agents',
    install: "pip install 'pilots-sdk[anthropic]'",
    note: "Anthropic runs the loop. Every tool call runs on a machine you own.",
  },
  {
    framework: 'TanStack AI',
    install: 'npm i @pilots/sdk',
    note: 'A sandbox provider with snapshots, exposed through @pilots/sdk/tanstack.',
  },
];
