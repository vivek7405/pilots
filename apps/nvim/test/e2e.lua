-- Drives the plugin end to end against a stand-in `pilot` binary.
--
-- The unit suite covers the pure halves; this covers the part that only a
-- real Neovim can run: the autocommands, the buffer rendering, and the read
-- and write round trip. It needs no fleet, because the stand-in runs each
-- guest command against a local directory instead of a machine.
--
-- Run: nvim -l apps/nvim/test/e2e.lua
local here = debug.getinfo(1, "S").source:sub(2):match("(.*)/test/e2e%.lua$")
vim.opt.runtimepath:prepend(here)
package.path = here .. "/lua/?.lua;" .. here .. "/lua/?/init.lua;" .. package.path

-- The stand-in runs guest commands on this machine, so the "guest
-- filesystem" is an ordinary directory addressed by its real absolute path.
-- Made here rather than passed in, so the suite is one command.
local root = os.getenv("PILOT_FAKE_ROOT") or vim.fn.tempname()
vim.fn.mkdir(root, "p")
local function u(path)
  return "pilot://m" .. root .. (path or "")
end
local passed, failed = 0, 0

local function check(name, fn)
  local ok, err = pcall(fn)
  if ok then
    passed = passed + 1
  else
    failed = failed + 1
    print("  FAIL " .. name .. "\n    " .. tostring(err))
  end
end

local function eq(got, want, what)
  if got ~= want then
    error((what or "value") .. ": got " .. vim.inspect(got) .. ", want " .. vim.inspect(want), 2)
  end
end

-- The fixtures are written HERE rather than by the caller, so the suite is one
-- command and a filename with a quote in it cannot be mangled by a shell on
-- the way in. That mangling already cost one debugging round.
local function put(path, body)
  local f = assert(io.open(root .. "/" .. path, "wb"))
  f:write(body)
  f:close()
end
vim.fn.mkdir(root .. "/sub", "p")
put("hello.txt", "one\ntwo\n")
put("noeol.txt", "no newline here")
put("empty.txt", "")
put("it's a file.txt", "quoted\n")
put("big.txt", "x\n")
put("binary.dat", "\0\1\2\255binary\0")
put("sub/nested.txt", "deep\n")

-- The plugin is loaded the way a plugin manager loads it.
dofile(here .. "/plugin/pilots.lua")
require("pilots").setup({ cmd = here .. "/test/fake-pilot.sh", home = root })

local function read_guest(path)
  local f = assert(io.open(root .. "/" .. path, "rb"))
  local body = f:read("*a")
  f:close()
  return body
end

check("a directory URL renders a listing, directories first", function()
  vim.cmd.edit(vim.fn.fnameescape(u()))
  local lines = vim.api.nvim_buf_get_lines(0, 0, -1, false)
  eq(lines[1], "../", "the climb entry must come first")
  eq(vim.bo.filetype, "pilots-listing")
  eq(vim.bo.modifiable, false, "a listing must not look editable")
  local joined = table.concat(lines, "\n")
  assert(joined:find("sub/", 1, true), "the directory is missing: " .. joined)
  assert(joined:find("hello.txt", 1, true), "the file is missing: " .. joined)
end)

check("a file opens with its bytes and no phantom trailing line", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/hello.txt")))
  eq(table.concat(vim.api.nvim_buf_get_lines(0, 0, -1, false), "\n"), "one\ntwo")
  eq(vim.bo.modified, false, "a freshly read buffer must be clean")
  eq(vim.bo.buftype, "acwrite", "without acwrite, :w never reaches the guest")
end)

check("writing a buffer lands the bytes in the guest, with its newline kept", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/hello.txt")))
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { "edited", "here" })
  vim.cmd.write()
  eq(read_guest("hello.txt"), "edited\nhere\n")
  eq(vim.bo.modified, false, "a successful write must clear modified")
end)

-- A file with no final newline must not gain one, and an empty file must not
-- become a one-byte file. Both are silent corruptions of someone's data.
check("a file with no trailing newline keeps not having one", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/noeol.txt")))
  vim.cmd.write()
  eq(read_guest("noeol.txt"), "no newline here")
end)

check("an empty file stays empty", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/empty.txt")))
  vim.cmd.write()
  eq(read_guest("empty.txt"), "")
end)

-- Names with a space and a single quote are ordinary, and unquoted they end
-- the shell string and run the rest as a command.
check("a name with a space and a quote round-trips", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/it's a file.txt")))
  eq(table.concat(vim.api.nvim_buf_get_lines(0, 0, -1, false), "\n"), "quoted")
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { "rewritten" })
  vim.cmd.write()
  eq(read_guest("it's a file.txt"), "rewritten\n")
end)

-- Linux caps a single argv string, so a payload past the bound fails the exec
-- itself. This is the assertion that the chunking actually reassembles.
check("a file larger than the payload bound round-trips byte for byte", function()
  local big = string.rep("abcdefghij", 12000) -- 120 KB, well past one exec
  vim.cmd.edit(vim.fn.fnameescape(u("/big.txt")))
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { big })
  vim.cmd.write()
  eq(read_guest("big.txt"), big .. "\n")
  vim.cmd.edit(vim.fn.fnameescape(u("/big.txt")))
  eq(vim.api.nvim_buf_get_lines(0, 0, -1, false)[1], big, "the read back differs")
end)

check("a binary file survives the round trip", function()
  vim.cmd.edit(vim.fn.fnameescape(u("/binary.dat")))
  vim.cmd.write()
  eq(read_guest("binary.dat"), "\0\1\2\255binary\0")
end)

check("following an entry descends, and .. climbs back", function()
  vim.cmd.edit(vim.fn.fnameescape(u()))
  -- Put the cursor on the directory entry and follow it.
  local lines = vim.api.nvim_buf_get_lines(0, 0, -1, false)
  for i, l in ipairs(lines) do
    if l == "sub/" then
      vim.api.nvim_win_set_cursor(0, { i, 0 })
    end
  end
  require("pilots").follow()
  eq(vim.api.nvim_buf_get_name(0), u("/sub"))
  vim.api.nvim_win_set_cursor(0, { 1, 0 }) -- the ../ entry
  require("pilots").follow()
  eq(vim.api.nvim_buf_get_name(0), u())
end)

print(string.format("\ne2e: %d passed, %d failed", passed, failed))
os.exit(failed == 0 and 0 or 1)
