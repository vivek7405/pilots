-- The autocommands that make `pilot://` a thing Neovim can open.
--
-- In plugin/ rather than behind setup(), so the plugin works the moment it is
-- installed: a user who types `:e pilot://scratch/` before configuring
-- anything should get their machine, not an empty buffer named after a URL.
--
-- Loaded once. A plugin manager that sources this twice would otherwise
-- register the group twice and every read would run two guest commands.
if vim.g.loaded_pilots then
  return
end

-- Refused by name rather than left to fail at the first keystroke.
--
-- The plugin needs `vim.system` and `vim.base64`, both 0.10. On 0.9 the first
-- read dies with "attempt to index field 'base64' (a nil value)" from inside
-- an autocommand, which names neither this plugin nor the reason, and the
-- buffer is already open by then.
if vim.fn.has("nvim-0.10") ~= 1 then
  vim.notify(
    "pilots.nvim needs Neovim 0.10 or newer: it uses vim.system and vim.base64",
    vim.log.levels.ERROR
  )
  return
end

vim.g.loaded_pilots = true

local group = vim.api.nvim_create_augroup("pilots", { clear = true })

vim.api.nvim_create_autocmd("BufReadCmd", {
  group = group,
  pattern = "pilot://*",
  callback = function(ev)
    require("pilots").read_buf(ev.buf, ev.match)
  end,
})

vim.api.nvim_create_autocmd("BufWriteCmd", {
  group = group,
  pattern = "pilot://*",
  callback = function(ev)
    require("pilots").write_buf(ev.buf, ev.match)
  end,
})

-- `<CR>` follows an entry, but ONLY in a listing buffer: the filetype is set
-- by the renderer, so this mapping cannot shadow Enter in a file the user is
-- editing.
vim.api.nvim_create_autocmd("FileType", {
  group = group,
  pattern = "pilots-listing",
  callback = function(ev)
    vim.keymap.set("n", "<CR>", function()
      require("pilots").follow()
    end, { buffer = ev.buf, desc = "pilots: open the entry under the cursor" })
  end,
})

vim.api.nvim_create_user_command("PilotsOpen", function(opts)
  require("pilots").open(opts.fargs[1], opts.fargs[2])
end, { nargs = "+", desc = "pilots: open a machine, or a path inside one" })

vim.api.nvim_create_user_command("PilotsTerminal", function(opts)
  require("pilots").terminal(opts.fargs[1])
end, { nargs = 1, desc = "pilots: a shell on a machine" })
