-- The test runner: `nvim -l apps/nvim/test/run.lua` from the repo root.
--
-- Plain Lua over busted on purpose. busted is a luarocks install, and a suite
-- that needs one more system dependency than the plugin itself is a suite CI
-- will eventually be allowed to skip. Neovim is already required to USE this
-- plugin, so running the tests in it costs nothing that was not already true.
local here = debug.getinfo(1, "S").source:sub(2):match("(.*)/test/run%.lua$")
package.path = here .. "/lua/?.lua;" .. here .. "/lua/?/init.lua;" .. package.path

local passed, failed = 0, 0
local failures = {}

function _G.test(name, fn)
  local ok, err = pcall(fn)
  if ok then
    passed = passed + 1
  else
    failed = failed + 1
    table.insert(failures, name .. "\n    " .. tostring(err))
  end
end

function _G.eq(got, want, what)
  if type(got) == "table" and type(want) == "table" then
    if vim.deep_equal(got, want) then
      return
    end
    error((what or "value") .. ":\n      got  " .. vim.inspect(got)
      .. "\n      want " .. vim.inspect(want), 2)
  end
  if got ~= want then
    error((what or "value") .. ": got " .. vim.inspect(got)
      .. ", want " .. vim.inspect(want), 2)
  end
end

function _G.truthy(cond, what)
  if not cond then
    error(what or "expected a truthy value", 2)
  end
end

for _, file in ipairs({ "url_spec", "guest_spec", "fs_spec" }) do
  dofile(here .. "/test/" .. file .. ".lua")
end

print(string.format("\n%d passed, %d failed", passed, failed))
for _, f in ipairs(failures) do
  print("  FAIL " .. f)
end
os.exit(failed == 0 and 0 or 1)
