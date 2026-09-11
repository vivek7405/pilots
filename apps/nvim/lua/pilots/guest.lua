--- What this plugin says to a guest, and how it reads the answer back.
---
--- A port of apps/vscode/src/guest.ts, deliberately one for one. Every comment
--- below records a bug that was already paid for once in the VS Code
--- extension, so the two must not drift: a fix in one is a fix owed to the
--- other. Free of any `vim` API, which is what makes it testable on its own.
local M = {}

--- The most base64 one command may carry.
---
--- Linux caps a SINGLE argv string at MAX_ARG_STRLEN (128 KiB), and the whole
--- command is one argument to `sh -c`, so a payload past that fails the exec
--- itself, which the guest agent reports as exit 127 with empty stderr: a
--- failure that names nothing. A multiple of 4, so every chunk is a complete
--- base64 group and decodes on its own.
M.MAX_PAYLOAD = 48000

--- Shell-quotes a value for the guest.
---
--- Single quotes, with an embedded quote closed, escaped and reopened. Every
--- path below goes through this: a file called `it's here.txt` is ordinary,
--- and unquoted it ends the string and runs the rest as shell.
---@param value string
---@return string
function M.quote(value)
  return "'" .. value:gsub("'", "'\\''") .. "'"
end

--- The commands, in one place, so a test asserts on what really runs.
M.cmd = {}

--- One call for everything a stat needs; `-c` never follows a symlink.
function M.cmd.stat(path)
  return "stat -c '%F|%s|%Y|%W' -- " .. M.quote(path)
end

function M.cmd.read(path)
  return "base64 -w0 -- " .. M.quote(path)
end

--- The first chunk of a write: the parent is created and the file truncated.
---
--- `$(dirname ...)` is quoted, or a path with a space in it makes the wrong
--- directory and the redirect then fails on the one it meant.
function M.cmd.write(path, base64)
  return "mkdir -p -- \"$(dirname -- " .. M.quote(path) .. ")\" && printf %s "
    .. M.quote(base64) .. " | base64 -d > " .. M.quote(path)
end

--- Every chunk after the first, appended. See MAX_PAYLOAD.
function M.cmd.append(path, base64)
  return "printf %s " .. M.quote(base64) .. " | base64 -d >> " .. M.quote(path)
end

function M.cmd.mkdir(path)
  return "mkdir -p -- " .. M.quote(path)
end

function M.cmd.remove(path, recursive)
  return (recursive and "rm -rf -- " or "rm -f -- ") .. M.quote(path)
end

function M.cmd.exists(path)
  return "test -e " .. M.quote(path)
end

--- A rename, and a refusal the caller can SEE.
---
--- `mv -n` exits 0 when it skips, so a non-overwriting rename onto an existing
--- file would report success while moving nothing, and the editor would show a
--- file the guest does not have. The destination is tested instead.
function M.cmd.rename(from, to, overwrite)
  if overwrite then
    return "mv -f -- " .. M.quote(from) .. " " .. M.quote(to)
  end
  return "if [ -e " .. M.quote(to) .. " ]; then echo 'mv: File exists' >&2; exit 1; fi; "
    .. "mv -- " .. M.quote(from) .. " " .. M.quote(to)
end

--- The listing AND each entry's type in ONE command.
---
--- A `stat` per entry would be one round trip per file, which is what makes a
--- remote filesystem feel broken on a directory of any size.
---
--- `cd` first, then `read`: `for f in $(ls)` word-splits and globs every name,
--- so one file called `my file.txt` became two entries and a directory called
--- `sub dir` became two files. `cd` also gives the command a real exit code
--- and a real stderr, which `for` over a silenced `ls` never had: a path that
--- does not exist used to render as an empty folder.
function M.cmd.list(path)
  return "cd -- " .. M.quote(path) .. " && ls -A | while IFS= read -r f; do "
    .. "if [ -d \"$f\" ]; then echo \"d|$f\"; "
    .. "elif [ -L \"$f\" ]; then echo \"l|$f\"; "
    .. "else echo \"f|$f\"; fi; done"
end

--- `stat -c %F` to a kind.
---
--- The order matters and the word "file" alone is NOT enough to decide with:
--- coreutils prints `character special file` and `block special file` for
--- device nodes, so a check for "file" would open /dev/sda in an editor as
--- though it were text. `regular` is the word that means a regular file.
---@param description string
---@return string one of "directory", "symlink", "file", "unknown"
function M.kind_of(description)
  if description:find("directory", 1, true) then
    return "directory"
  end
  if description:find("symbolic link", 1, true) then
    return "symlink"
  end
  if description:find("special", 1, true)
    or description:find("fifo", 1, true)
    or description:find("socket", 1, true) then
    return "unknown"
  end
  if description:find("regular", 1, true) then
    return "file"
  end
  return "unknown"
end

--- Parses `cmd.stat`'s output, or nil when it did not answer.
---@param stdout string
---@return table|nil `{ kind, size, mtime, ctime }`, seconds
function M.parse_stat(stdout)
  local description, size, mtime, ctime = stdout:match("^%s*([^|]*)|([^|]*)|([^|]*)|([^|]*)%s*$")
  if description == nil or description == "" then
    return nil
  end
  -- A birth time of -1 is "not recorded", which is most filesystems.
  local birth = tonumber(ctime) or 0
  return {
    kind = M.kind_of(description),
    size = tonumber(size) or 0,
    mtime = tonumber(mtime) or 0,
    ctime = math.max(0, birth),
  }
end

--- Parses `cmd.list`'s output into `{ name = , kind = }` entries.
---
--- Split on the FIRST separator only, because a file may legitimately be
--- called `a|b` and splitting on every one would lose half its name.
---@param stdout string
---@return table[]
function M.parse_listing(stdout)
  local out = {}
  for line in (stdout .. "\n"):gmatch("([^\n]*)\n") do
    local at = line:find("|", 1, true)
    if at ~= nil then
      local marker, name = line:sub(1, at - 1), line:sub(at + 1)
      if name ~= "" then
        local kind = "file"
        if marker == "d" then
          kind = "directory"
        elseif marker == "l" then
          kind = "symlink"
        end
        table.insert(out, { name = name, kind = kind })
      end
    end
  end
  return out
end

--- Splits a base64 payload into chunks a single exec can carry.
---
--- Returns one empty chunk for an empty payload, so that writing an empty file
--- still truncates the target rather than leaving whatever was there.
---@param base64 string
---@return string[]
function M.chunks(base64)
  if #base64 == 0 then
    return { "" }
  end
  local out = {}
  local at = 1
  while at <= #base64 do
    table.insert(out, base64:sub(at, at + M.MAX_PAYLOAD - 1))
    at = at + M.MAX_PAYLOAD
  end
  return out
end

--- What the guest's stderr means.
---
--- A permission error rendered as "file not found" sends someone looking for a
--- path that is right there, so the two are told apart by what the shell said.
---@param stderr string
---@return string one of "not_found", "exists", "permission", "not_a_directory", "failed"
function M.classify(stderr)
  local s = stderr:lower()
  if s:find("no such file", 1, true) then
    return "not_found"
  end
  if s:find("file exists", 1, true) then
    return "exists"
  end
  if s:find("permission denied", 1, true) then
    return "permission"
  end
  if s:find("not a directory", 1, true) then
    return "not_a_directory"
  end
  return "failed"
end

return M
