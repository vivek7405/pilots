local fs = require("pilots.fs")

-- Directories first, then by name: the order every file manager uses, and the
-- one that makes a deep tree navigable by eye.
test("a listing renders directories first, with a type suffix", function()
  local lines = fs.listing_lines({
    { name = "zz", kind = "directory" },
    { name = "aa", kind = "file" },
    { name = "ln", kind = "symlink" },
  })
  eq(lines, { "../", "zz/", "aa", "ln@" })
end)

test("the entry name comes back without its suffix", function()
  eq(fs.entry_of("src/"), "src")
  eq(fs.entry_of("link@"), "link")
  eq(fs.entry_of("plain.txt"), "plain.txt")
  eq(fs.entry_of("../"), "..")
  eq(fs.entry_of(""), nil)
end)

-- A name that legitimately ends in @ or / inside the guest would round-trip
-- wrong. Both are legal in a POSIX name except /, so only @ is a real risk,
-- and this records that we know it.
test("a name ending in @ is ambiguous, and the suffix wins", function()
  eq(fs.entry_of("weird@"), "weird")
end)
