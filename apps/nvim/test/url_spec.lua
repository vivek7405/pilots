local url = require("pilots.url")

-- A bare machine name is the useful thing to type, so it means home. One
-- slash means the guest root, and the difference is load-bearing: if both
-- meant home, climbing out of /home would land back in home and the root
-- would be unreachable.
test("a bare machine is home, and a single slash is the root", function()
  eq(url.parse("pilot://scratch"), { machine = "scratch", path = "/home/pilot" })
  eq(url.parse("pilot://scratch/"), { machine = "scratch", path = "/" })
end)

test("a path is kept as given", function()
  eq(url.parse("pilot://scratch/etc/hosts"), { machine = "scratch", path = "/etc/hosts" })
end)

-- The split on the first slash is unambiguous because a machine name is a DNS
-- label and cannot contain one. If that ever stops being true, this is the
-- test that says so.
test("the machine is everything before the first slash", function()
  eq(url.parse("pilot://a-b-c1/x/y/z").machine, "a-b-c1")
  eq(url.parse("pilot://a-b-c1/x/y/z").path, "/x/y/z")
end)

test("a path with a space survives parsing", function()
  eq(url.parse("pilot://m/home/pilot/my file.txt").path, "/home/pilot/my file.txt")
end)

-- Two spellings of one path must be one buffer. Two buffers over one file can
-- disagree about what is on disk, and the second :w silently wins.
test("repeated and trailing slashes collapse", function()
  eq(url.parse("pilot://m//home//pilot//").path, "/home/pilot")
  eq(url.format("m", "/home//pilot/"), "pilot://m/home/pilot")
end)

test("what is not a pilot url is refused, with a reason", function()
  local parsed, err = url.parse("file:///etc/hosts")
  eq(parsed, nil)
  truthy(err ~= nil and err:find("pilot"), "the error should name the scheme")
  eq((url.parse("pilot://")), nil)
  eq((url.parse(nil)), nil)
end)

test("format is the inverse of parse", function()
  for _, u in ipairs({ "pilot://m/etc/hosts", "pilot://m/home/pilot/a b", "pilot://m/home/pilot", "pilot://m/" }) do
    local p = url.parse(u)
    eq(url.format(p.machine, p.path), u)
  end
end)

-- A tree you can descend and not climb is worse than one you cannot descend.
test("join climbs on .. and descends on a name", function()
  eq(url.join("pilot://m/home/pilot", "src"), "pilot://m/home/pilot/src")
  eq(url.join("pilot://m/home/pilot", ".."), "pilot://m/home")
  -- The whole way out, ending at the root and staying there rather than
  -- wrapping back to home.
  eq(url.join("pilot://m/home", ".."), "pilot://m/")
  eq(url.join("pilot://m/", ".."), "pilot://m/")
end)

test("a path with a single quote survives both directions", function()
  local u = url.format("m", "/home/pilot/it's here.txt")
  eq(url.parse(u).path, "/home/pilot/it's here.txt")
end)
