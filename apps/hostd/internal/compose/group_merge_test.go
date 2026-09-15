package compose

import (
	"strings"
	"testing"
)

// A grouped machine carries every member's volume, not the first member's.
//
// mergeMembers starts from a copy of the alphabetically-first member and used
// to overwrite only Env, Ports, Health, DependsOn and the sizes. Volumes was
// never gathered, so `web` and `worker` on one build context, where worker
// declares a volume and web does not, deployed a machine with NO volume: every
// byte the worker wrote landed on the copy-on-write rootfs and died with the
// next redeploy, with nothing in the output saying a declaration had been
// dropped. Reverting the gather makes this test report zero volumes.
func TestEveryMembersVolumeReachesTheMachine(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
volumes:
  queue:
  cache:
services:
  web:
    build: .
    command: node server.js
    volumes:
      - cache:/var/cache
  worker:
    build: .
    command: node worker.js
    volumes:
      - queue:/var/lib/queue
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("got %d steps, want one machine", len(plan.Steps))
	}
	got := map[string]string{}
	for _, v := range plan.Steps[0].Volumes {
		got[v.MountPath] = v.Name
	}
	if len(got) != 2 || got["/var/cache"] == "" || got["/var/lib/queue"] == "" {
		t.Errorf("volumes = %+v, want both members' volumes; the worker's is the "+
			"one that used to be dropped", plan.Steps[0].Volumes)
	}
}

// Two members mounting one path from different volumes is refused, not
// resolved. One machine has one filesystem, so picking either silently is the
// same data-loss bug wearing a successful deploy.
func TestTwoMembersClaimingOneMountPathAreRefused(t *testing.T) {
	_, planErr := planText(t, `
name: demo
volumes:
  alpha:
  beta:
services:
  web:
    build: .
    command: node server.js
    volumes:
      - alpha:/data
  worker:
    build: .
    command: node worker.js
    volumes:
      - beta:/data
`)
	if planErr == nil {
		t.Fatal("two members claimed one mount path from different volumes and the plan was accepted")
	}
	if !strings.Contains(planErr.Error, "/data") {
		t.Errorf("the refusal does not name the mount path: %q", planErr.Error)
	}
}

// A member with no command is refused at PLAN time, naming the service.
//
// It used to be accepted, and the plan then carried a process entry with an
// empty Cmd. declaredProcesses in the guest agent rejects that entry and
// rejects the WHOLE list with it, so the machine started nothing at all --
// neither this member nor the others -- and said so in a message that names no
// service and never mentions compose.
func TestAMemberWithNoCommandIsRefusedByName(t *testing.T) {
	_, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    command: node server.js
  worker:
    build: .
`)
	if planErr == nil {
		t.Fatal("a grouped member declared no command and the plan was accepted; the machine it describes starts nothing")
	}
	if !strings.Contains(planErr.Error, "worker") || !strings.Contains(planErr.Error, "command") {
		t.Errorf("the refusal does not name the service and the fix: %q", planErr.Error)
	}
}

// A conflict the file really has beats the missing-command message, so the
// user is told the thing only they can decide.
func TestAConflictIsReportedBeforeAMissingCommand(t *testing.T) {
	_, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    environment:
      DATABASE_URL: postgres://a
  worker:
    build: .
    environment:
      DATABASE_URL: postgres://b
`)
	if planErr == nil {
		t.Fatal("a conflicting env key was accepted")
	}
	if !strings.Contains(planErr.Error, "DATABASE_URL") {
		t.Errorf("the missing command masked the real conflict: %q", planErr.Error)
	}
}

// An exec-form command survives becoming a shell string.
//
// shellFromJSONArray used to split on commas and join on a space, which
// destroyed every argument containing a space or a comma. The postgres recipe
// has one -- archive_command=test ! -f ... && cp %p ... -- and recipes.go chose
// a LIST for it precisely because "a string would be re-split by whatever runs
// it". This was the code re-splitting it: the flattened string reached the
// guest's `sh -c`, which split it again on the &&, and postgres exited with
// "too many command-line arguments". Restoring the comma split reds every case
// below except the first and the last.
func TestAnExecFormCommandSurvivesFlattening(t *testing.T) {
	for _, tc := range []struct{ append, want string }{
		{`CMD ["node","server.js"]`, "node server.js"},
		// A space inside one argument. The comma split turned this into four
		// bare words.
		{`CMD ["sh","-c","echo hello world"]`, `sh -c 'echo hello world'`},
		// A comma inside one argument, which the split tore in half.
		{`CMD ["psql","-c","SELECT a, b FROM t"]`, `psql -c 'SELECT a, b FROM t'`},
		// The real one, from the postgres recipe.
		{`CMD ["postgres","-c","archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f"]`,
			`postgres -c 'archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f'`},
		// An embedded single quote closes, escapes and reopens.
		{`CMD ["sh","-c","echo it's fine"]`, `sh -c 'echo it'\''s fine'`},
		// An empty argument is a real argument and must not vanish.
		{`CMD ["prog","","x"]`, `prog '' x`},
		// Shell form is already a shell string and is handed back as written.
		{`CMD node server.js`, "node server.js"},
	} {
		if got := commandOf(Step{DockerfileAppend: tc.append}); got != tc.want {
			t.Errorf("commandOf(%q)\n = %q\nwant %q", tc.append, got, tc.want)
		}
	}
}

// A member that named an `image:` has its command in the generated Dockerfile,
// not in the append. Reading only the append gave it an empty command, which
// is the refusal above firing on a file that declared one perfectly well.
func TestAnImageStepsOwnCommandIsFound(t *testing.T) {
	s := Step{Dockerfile: "FROM redis:7\nCMD [\"redis-server\",\"--appendonly\",\"yes\"]\n"}
	if got := commandOf(s); got != "redis-server --appendonly yes" {
		t.Errorf("commandOf(image step) = %q, want the image's own CMD", got)
	}
}

// The LAST CMD wins, which is Docker's own rule. Reading the first handed back
// the line the override was replacing.
func TestTheLastCommandWins(t *testing.T) {
	s := Step{DockerfileAppend: "CMD [\"old\"]\nWORKDIR /app\nCMD [\"new\"]\n"}
	if got := commandOf(s); got != "new" {
		t.Errorf("commandOf = %q, want the last CMD", got)
	}
}
