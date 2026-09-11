--- pilots for Neovim: a machine is a folder you are editing.
---
--- Neovim has no FileSystemProvider, so a `pilot://` buffer is built from
--- autocommands on a URL pattern, the way netrw, fugitive and oil.nvim all do
--- it. The events are in plugin/pilots.lua so the plugin works with no
--- setup() call; this module holds what they do.
local fs = require("pilots.fs")
local url = require("pilots.url")

local M = {}

--- Options a user may override through setup().
M.options = {
  --- The `pilot` binary, when it is not on PATH.
  cmd = "pilot",
  --- Where `:PilotsOpen <machine>` starts with no path given.
  home = url.default_home,
  --- How long one guest command may take, in milliseconds.
  timeout_ms = 60000,
}

---@param opts table|nil
function M.setup(opts)
  M.options = vim.tbl_extend("force", M.options, opts or {})
  fs.cmd_name = M.options.cmd
  fs.timeout_ms = M.options.timeout_ms
  url.default_home = M.options.home
end

--- Fills a buffer from the guest: a file's bytes, or a directory's listing.
---
--- Both kinds of URL land here, because the editor cannot know which it has
--- until something asks the guest. One stat answers that, and a stat is the
--- cheapest question available.
---@param bufnr integer
---@param target string
function M.read_buf(bufnr, target)
  local parsed, perr = url.parse(target)
  if parsed == nil then
    vim.notify("pilots: " .. tostring(perr), vim.log.levels.ERROR)
    return
  end

  local st, serr = fs.stat(parsed.machine, parsed.path)
  if serr ~= nil then
    vim.notify("pilots: " .. serr, vim.log.levels.ERROR)
    return
  end

  if st.kind == "directory" then
    local err = fs.render_listing(bufnr, target)
    if err ~= nil then
      vim.notify("pilots: " .. err, vim.log.levels.ERROR)
    end
    return
  end

  local bytes, rerr = fs.read(parsed.machine, parsed.path)
  if rerr ~= nil then
    vim.notify("pilots: " .. rerr, vim.log.levels.ERROR)
    return
  end

  -- split("\n") on content ending in a newline yields a trailing empty
  -- element, which is exactly what Neovim wants: a POSIX file's final newline
  -- is a terminator, not a separator, and dropping it would rewrite every
  -- file that has one the first time it is saved.
  local lines = vim.split(bytes, "\n", { plain = true })
  if bytes == "" then
    -- An empty file is NOT a file with one empty line. A Neovim buffer always
    -- has at least one line, so without this the round trip writes back a
    -- single newline and a zero-byte file silently becomes one byte.
    vim.b[bufnr].pilots_no_eol = true
    lines = { "" }
  elseif lines[#lines] == "" then
    table.remove(lines)
  else
    -- No trailing newline in the guest, so do not add one back on write.
    vim.b[bufnr].pilots_no_eol = true
  end

  vim.bo[bufnr].modifiable = true
  vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, lines)
  vim.bo[bufnr].modified = false
  vim.bo[bufnr].buftype = "acwrite"
  -- Let the usual detection run, so syntax and LSP-free niceties still work
  -- on a buffer whose name is a URL.
  vim.api.nvim_buf_call(bufnr, function()
    vim.cmd("filetype detect")
  end)
end

--- Pushes a buffer's contents into the guest.
---@param bufnr integer
---@param target string
function M.write_buf(bufnr, target)
  local parsed, perr = url.parse(target)
  if parsed == nil then
    vim.notify("pilots: " .. tostring(perr), vim.log.levels.ERROR)
    return
  end

  local lines = vim.api.nvim_buf_get_lines(bufnr, 0, -1, false)
  local bytes = table.concat(lines, "\n")
  if not vim.b[bufnr].pilots_no_eol then
    bytes = bytes .. "\n"
  end

  local err = fs.write(parsed.machine, parsed.path, bytes)
  if err ~= nil then
    -- Left modified on purpose. A buffer marked clean after a failed write
    -- is how an edit is lost: the next :q asks nothing and the change is gone.
    vim.notify("pilots: " .. err, vim.log.levels.ERROR)
    return
  end
  vim.bo[bufnr].modified = false
end

--- Opens a machine, or a path inside one.
---@param machine string
---@param path string|nil
function M.open(machine, path)
  vim.cmd.edit(vim.fn.fnameescape(url.format(machine, path or M.options.home)))
end

--- Follows the entry under the cursor in a listing buffer.
function M.follow()
  local target = vim.api.nvim_buf_get_name(0)
  local name = fs.entry_of(vim.api.nvim_get_current_line())
  if name == nil then
    return
  end
  local next_url = url.join(target, name)
  if next_url ~= nil then
    vim.cmd.edit(vim.fn.fnameescape(next_url))
  end
end

--- A shell in the machine, beside the files.
---@param machine string
function M.terminal(machine)
  vim.cmd("botright split")
  vim.cmd.terminal(M.options.cmd .. " console " .. vim.fn.shellescape(machine))
  vim.cmd.startinsert()
end

return M
