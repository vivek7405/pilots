local guest = require("pilots.guest")

-- A file called `it's here.txt` is ordinary. Unquoted it ends the string and
-- runs the rest of the line as shell.
test("quote survives a single quote in a name", function()
  eq(guest.quote("it's here.txt"), [['it'\''s here.txt']])
  eq(guest.quote("plain"), "'plain'")
end)

test("every command quotes its path", function()
  local path = "/home/pilot/a b/it's.txt"
  for _, c in ipairs({
    guest.cmd.stat(path), guest.cmd.read(path), guest.cmd.mkdir(path),
    guest.cmd.exists(path), guest.cmd.list(path), guest.cmd.remove(path, true),
  }) do
    truthy(c:find(guest.quote(path), 1, true) ~= nil, "unquoted path in: " .. c)
  end
end)

-- `for f in $(ls)` word-splits and globs every name, so `my file.txt` became
-- two entries and `sub dir` became two directories.
test("the listing reads names a line at a time, never by word-splitting", function()
  local c = guest.cmd.list("/tmp")
  truthy(c:find("IFS= read -r", 1, true) ~= nil, "the listing must read line by line")
  truthy(c:find("cd --", 1, true) ~= nil, "cd gives the command a real exit code")
  truthy(c:find("for f in", 1, true) == nil, "word-splitting is the bug this replaced")
end)

-- A path with a space in it makes the wrong directory when the subshell is
-- unquoted, and the redirect then fails on the one it meant.
test("the write quotes its dirname subshell", function()
  local c = guest.cmd.write("/home/pilot/a b/f.txt", "AAAA")
  truthy(c:find('"$(dirname', 1, true) ~= nil, "the subshell must be quoted: " .. c)
end)

-- `mv -n` exits 0 when it skips, so a non-overwriting rename onto an existing
-- file would report success while moving nothing.
test("a non-overwriting rename tests the destination rather than trusting mv -n", function()
  local c = guest.cmd.rename("/a", "/b", false)
  truthy(c:find("mv -n", 1, true) == nil, "mv -n reports success when it skips")
  truthy(c:find("File exists", 1, true) ~= nil, "the refusal must be visible")
  truthy(guest.cmd.rename("/a", "/b", true):find("mv -f", 1, true) ~= nil)
end)

-- A check for "file" would open /dev/sda in an editor as though it were text:
-- coreutils prints `character special file` for a device node.
test("a device node is not a regular file", function()
  eq(guest.kind_of("regular file"), "file")
  eq(guest.kind_of("regular empty file"), "file")
  eq(guest.kind_of("directory"), "directory")
  eq(guest.kind_of("symbolic link"), "symlink")
  eq(guest.kind_of("character special file"), "unknown")
  eq(guest.kind_of("block special file"), "unknown")
  eq(guest.kind_of("fifo"), "unknown")
  eq(guest.kind_of("socket"), "unknown")
end)

test("stat parses, and says nothing rather than guessing when it did not answer", function()
  eq(guest.parse_stat("regular file|1234|1700000000|-1"),
    { kind = "file", size = 1234, mtime = 1700000000, ctime = 0 })
  eq(guest.parse_stat(""), nil)
  eq(guest.parse_stat("\n"), nil)
end)

-- A file may legitimately be called `a|b`, and splitting on every separator
-- would lose half its name.
test("a listing splits on the first separator only", function()
  local got = guest.parse_listing("d|src\nf|a|b\nl|link\n\ngarbage\n")
  eq(got, {
    { name = "src", kind = "directory" },
    { name = "a|b", kind = "file" },
    { name = "link", kind = "symlink" },
  })
end)

-- Linux caps a single argv string at MAX_ARG_STRLEN, and the whole command is
-- one argument to `sh -c`. Past that the exec itself fails, which the guest
-- agent reports as exit 127 with empty stderr: a failure that names nothing.
-- The bound is asserted by VALUE, and the payload below is a fixed size that
-- does not derive from it. Sizing the payload as MAX_PAYLOAD + 1 would make
-- this test follow the constant wherever it went and it could never fail,
-- which is the same as not testing the bound at all.
test("the payload bound is 48000: under MAX_ARG_STRLEN and a whole base64 group", function()
  eq(guest.MAX_PAYLOAD, 48000)
  eq(guest.MAX_PAYLOAD % 4, 0, "a chunk that is not a whole base64 group cannot decode alone")
  truthy(guest.MAX_PAYLOAD < 128 * 1024, "a single argv string is capped at MAX_ARG_STRLEN")
end)

test("a fixed 96001-byte payload becomes three chunks that reassemble exactly", function()
  local payload = string.rep("A", 96001)
  local chunks = guest.chunks(payload)
  eq(#chunks, 3, "96001 bytes over a 48000 bound is three chunks")
  eq(#chunks[1], 48000)
  eq(#chunks[2], 48000)
  eq(#chunks[3], 1)
  eq(table.concat(chunks), payload, "the chunks must reassemble exactly")
end)

test("a payload at the bound is one chunk, and an empty one still truncates", function()
  eq(#guest.chunks(string.rep("A", 48000)), 1)
  eq(#guest.chunks(string.rep("A", 48001)), 2)
  eq(guest.chunks(""), { "" })
end)

-- A permission error rendered as "file not found" sends someone looking for a
-- path that is right there.
test("stderr is classified by what the shell actually said", function()
  eq(guest.classify("stat: cannot stat '/x': No such file or directory"), "not_found")
  eq(guest.classify("mv: File exists"), "exists")
  eq(guest.classify("cat: /root/x: Permission denied"), "permission")
  eq(guest.classify("cd: /etc/hosts: Not a directory"), "not_a_directory")
  eq(guest.classify("something else entirely"), "failed")
end)
