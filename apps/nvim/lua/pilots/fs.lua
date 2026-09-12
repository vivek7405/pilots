--- Reading and writing a guest's filesystem through the `pilot` CLI.
---
--- Everything here shells out to `pilot exec`, which already solves
--- authentication, machine-name resolution and the exec route
--- (apps/pilot/internal/cli/console.go). Talking to the API directly from Lua
--- would duplicate all three for nothing.
local guest = require("pilots.guest")
local url = require("pilots.url")

local M = {}

--- The `pilot` binary. Overridable, because a developer running from a
--- checkout has one that is not on PATH.
M.cmd_name = "pilot"

--- How long one guest command may take.
---
--- A read of a large file over a slow link is the long case; past this the
--- editor should say so rather than appear to hang, because a hung `:w` looks
--- exactly like a lost edit.
M.timeout_ms = 60000

--- Runs one shell command inside a machine.
---
--- The command is passed as a single argv entry after `--`, so the CLI hands
--- it to the guest verbatim and nothing on this side re-splits it. Everything
--- that builds those strings quotes its own arguments; see guest.quote.
---@param machine string
---@param command string
---@return table `{ code = number, stdout = string, stderr = string }`
function M.exec(machine, command)
  local argv = { M.cmd_name, "exec", machine, "--", "sh", "-c", command }
  local ok, res = pcall(function()
    return vim.system(argv, { text = true }):wait(M.timeout_ms)
  end)
  if not ok then
    -- A missing binary is the common case and its message says only
    -- "ENOENT", which names nothing a user can act on.
    return { code = 127, stdout = "", stderr = tostring(res) .. " (is `" .. M.cmd_name .. "` on PATH?)" }
  end
  return { code = res.code or 0, stdout = res.stdout or "", stderr = res.stderr or "" }
end

--- Reads a guest file and returns its bytes.
---@param machine string
---@param path string
---@return string|nil bytes
---@return string|nil err
function M.read(machine, path)
  local res = M.exec(machine, guest.cmd.read(path))
  if res.code ~= 0 then
    return nil, guest.classify(res.stderr) .. ": " .. path
  end
  local decoded = vim.base64.decode((res.stdout:gsub("%s+", "")))
  return decoded, nil
end

--- Writes bytes to a guest file, creating parent directories.
---
--- Chunked at guest.MAX_PAYLOAD: the first chunk truncates, the rest append.
--- A failure part way leaves a partial file, which is the same thing a failed
--- write to a local disk leaves, and is why the error says which chunk.
---@param machine string
---@param path string
---@param bytes string
---@return string|nil err
function M.write(machine, path, bytes)
  local chunks = guest.chunks(vim.base64.encode(bytes))
  for i, chunk in ipairs(chunks) do
    local command = i == 1 and guest.cmd.write(path, chunk) or guest.cmd.append(path, chunk)
    local res = M.exec(machine, command)
    if res.code ~= 0 then
      return guest.classify(res.stderr) .. ": " .. path
        .. " (chunk " .. i .. " of " .. #chunks .. ")"
    end
  end
  return nil
end

--- Lists a directory in one round trip.
---@param machine string
---@param path string
---@return table[]|nil entries
---@return string|nil err
function M.list(machine, path)
  local res = M.exec(machine, guest.cmd.list(path))
  if res.code ~= 0 then
    return nil, guest.classify(res.stderr) .. ": " .. path
  end
  local entries = guest.parse_listing(res.stdout)
  table.sort(entries, function(a, b)
    -- Directories first, then by name. The same order every file manager
    -- uses, and the one that makes a deep tree navigable by eye.
    if (a.kind == "directory") ~= (b.kind == "directory") then
      return a.kind == "directory"
    end
    return a.name < b.name
  end)
  return entries, nil
end

--- Stats a guest path.
---
--- `follow` asks about the TARGET of a symlink rather than the link. That is
--- what opening needs: `cmd.list` already marks a symlinked directory `d`,
--- because `[ -d ]` dereferences, so without it the listing says directory and
--- the open says file and `<CR>` on an ordinary `current -> releases/x` fails.
---@param machine string
---@param path string
---@param follow boolean|nil
---@return table|nil stat
---@return string|nil err
function M.stat(machine, path, follow)
  local command = follow and guest.cmd.stat_target(path) or guest.cmd.stat(path)
  local res = M.exec(machine, command)
  if res.code ~= 0 then
    return nil, guest.classify(res.stderr) .. ": " .. path
  end
  local parsed = guest.parse_stat(res.stdout)
  if parsed == nil then
    return nil, "not_found: " .. path
  end
  return parsed, nil
end

--- The lines a listing buffer shows.
---
--- `..` is always first and always present, including at the root, where it
--- resolves to the root again rather than disappearing: a tree you can descend
--- and not climb is worse than one you cannot descend.
---@param entries table[]
---@return string[]
function M.listing_lines(entries)
  local lines = { "../" }
  for _, e in ipairs(entries) do
    if e.kind == "directory" then
      table.insert(lines, e.name .. "/")
    elseif e.kind == "symlink" then
      table.insert(lines, e.name .. "@")
    else
      table.insert(lines, e.name)
    end
  end
  return lines
end

--- The entry name a listing line refers to, with the type suffix removed.
---
--- A FALLBACK only. The rendered line is lossy: a regular file named `at@`
--- renders with no suffix of its own and this strips the `@` anyway, so the
--- caller opens a path that does not exist. `render_listing` records the
--- parsed entries on the buffer and `follow` reads those instead; this is what
--- answers when a line has no entry behind it, which is the `../` row and
--- anything a user typed into the buffer.
---@param line string
---@return string|nil
function M.entry_of(line)
  local name = line:gsub("[/@]$", "")
  if name == "" then
    return nil
  end
  return name
end

--- The entry a listing line number refers to, from what was parsed rather
--- than from what was drawn.
---
--- Line 1 is always `../`; entry N is on line N+1.
---@param bufnr integer
---@param lnum integer 1-based
---@return string|nil
function M.entry_at(bufnr, lnum)
  if lnum <= 1 then
    return ".."
  end
  local ok, entries = pcall(vim.api.nvim_buf_get_var, bufnr, "pilots_entries")
  if ok and type(entries) == "table" and entries[lnum - 1] ~= nil then
    return entries[lnum - 1].name
  end
  return M.entry_of(vim.api.nvim_buf_get_lines(bufnr, lnum - 1, lnum, false)[1] or "")
end

--- Renders a directory into the current buffer.
---@param bufnr integer
---@param target string the pilot:// url of the directory
---@return string|nil err
function M.render_listing(bufnr, target)
  local parsed = url.parse(target)
  if parsed == nil then
    return "not a pilot:// url: " .. tostring(target)
  end
  local entries, err = M.list(parsed.machine, parsed.path)
  if err ~= nil then
    return err
  end
  vim.bo[bufnr].modifiable = true
  vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, M.listing_lines(entries))
  -- The parsed entries, kept so that following one does not have to reverse
  -- the rendering. See entry_at.
  vim.api.nvim_buf_set_var(bufnr, "pilots_entries", entries)
  vim.bo[bufnr].modifiable = false
  vim.bo[bufnr].modified = false
  vim.bo[bufnr].buftype = "nofile"
  vim.bo[bufnr].filetype = "pilots-listing"
  return nil
end

return M
