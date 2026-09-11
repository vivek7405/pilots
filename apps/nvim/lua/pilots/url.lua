--- Parsing and rendering of `pilot://<machine>/<path>`.
---
--- Kept separate from everything that talks to a guest, because this half is
--- pure and is where the tests are. The same split `apps/vscode` makes between
--- `guest.ts` and `filesystem.ts`, and for the same reason: the testable half
--- should not need an editor, or in that case an extension host, to run.
local M = {}

--- The scheme every buffer in a machine carries.
M.scheme = "pilot://"

--- Where `:PilotsOpen <machine>` starts when no path is given.
---
--- Matches the `pilots.home` default in apps/vscode/package.json. The two are
--- the same promise to a user who moves between editors, so they move together.
M.default_home = "/home/pilot"

--- Splits a pilot URL into its machine and its absolute guest path.
---
--- The split on the first slash is unambiguous rather than lucky: a machine
--- name is a DNS label (apps/hostd/internal/machines/name.go), so it cannot
--- contain a slash.
---
--- `pilot://m` with NO slash is the home directory, which is what makes a bare
--- machine name a useful thing to type. `pilot://m/` with one IS the guest
--- root, and the difference is load-bearing: if both meant home, climbing out
--- of /home would land back in home and the root would be unreachable, so a
--- tree you can descend would have a floor you cannot cross.
---
---@param url string
---@return table|nil parsed `{ machine = string, path = string }`, or nil
---@return string|nil err why it was not a pilot URL
function M.parse(url)
  if type(url) ~= "string" then
    return nil, "not a string"
  end
  local rest = url:match("^" .. M.scheme:gsub("%p", "%%%0") .. "(.*)$")
  if rest == nil then
    return nil, "not a pilot:// url"
  end

  local machine, path = rest:match("^([^/]+)(.*)$")
  if machine == nil or machine == "" then
    return nil, "no machine in the url"
  end
  if path == nil or path == "" then
    path = M.default_home
  end
  -- Collapse the repeated slashes a naive concatenation produces, so that
  -- `pilot://m//home` and `pilot://m/home` are one buffer rather than two
  -- views of one file that can disagree about what is on disk.
  path = path:gsub("//+", "/")
  if path ~= "/" then
    path = path:gsub("/$", "")
  end
  return { machine = machine, path = path }
end

--- The inverse of parse.
---@param machine string
---@param path string
---@return string
function M.format(machine, path)
  if path == nil or path == "" then
    path = M.default_home
  end
  if path == "/" then
    -- The root is one slash and must stay one, or format would emit a bare
    -- `pilot://m`, which parse reads back as home.
    return M.scheme .. machine .. "/"
  end
  if path:sub(1, 1) ~= "/" then
    path = "/" .. path
  end
  path = path:gsub("//+", "/")
  if path ~= "/" then
    path = path:gsub("/$", "")
  end
  return M.scheme .. machine .. path
end

--- The URL of a named entry inside a directory URL.
---
--- Used by the listing buffer, where `..` has to climb rather than append,
--- or a wrong turn in a tree leaves no way back up.
---@param url string the directory
---@param name string the entry, or ".."
---@return string|nil
function M.join(url, name)
  local parsed = M.parse(url)
  if parsed == nil then
    return nil
  end
  if name == ".." then
    local up = parsed.path:match("^(.*)/[^/]*$")
    if up == nil or up == "" then
      up = "/"
    end
    return M.format(parsed.machine, up)
  end
  local base = parsed.path
  if base:sub(-1) ~= "/" then
    base = base .. "/"
  end
  return M.format(parsed.machine, base .. name)
end

return M
