package server

import "strings"

// helpOverview 是空调用与 help 的默认输出：有什么、怎么选。
//
// 它要回答的不是"有哪些命令"，而是"我现在该用哪个"——
// 一个只列出动词名的手册，看完还是要猜。
func helpOverview() string {
	return strings.TrimSpace(`
zero-memcp: persistent memory. Writes are stored locally and remain available after restart.

put [<tree>] -- $TEXT        write content to a memory tree (pass content through vars/args, separately from the DSL)
put <tree> --ref <addr>      add a reference to existing memory in a tree without copying its content
recall <text> [--scope tree] semantic search; searches long-term memory by default, with an optional scope
read <addr> [<addr>...]     read full text; accepts multiple addresses
grep <pattern> [--scope T]  search full text with a regular expression (exact symbols, names, numbers)
log [N]                     timeline of recent memories
ls [date]                   list memory entries for a date
trees <project>[@<branch>]  view the tree directory
remind [<tree>]             watch a tree and receive notifications when it changes
since <tree> <commit|empty> retrieve tree changes since the last seen commit
status                      index size

Memory sync (remotes are declared in the config):
remote [<project>]                    list configured memory remotes
fetch <project> [<remote>]            fetch history from a remote without automatically overwriting local branches
push <project> [<remote>] <branch>   explicitly push one branch (one at a time; see README for external git push)
sync <project> [<remote>]             compare local and remote state
remote-key list | put | delete       root manages SSH private keys (for external push)
remote-secret list | put | delete    root manages HTTP tokens (for external push)

push requires an explicitly named branch; it does not commit or force-push for you.

Commands for maintaining memory structure (not needed for routine reading and writing):
commit [<p>[@<br>]] [-m msg]   explicitly fold the dirty layer into history
diff <p>@<br> [-> <p>@<br>]    compare branch differences (for auditing)
branch [<p> [<new> [from s]]]  list or fork branches
tag [-d] <p> [<name> [<ref>]]  lightweight tag
cp / mv <src> -> <dst>         transfer entries across branches
delete <p>@<br>/<path>...      delete entries
archive <p>@<br>               archive a branch
reset <p>@<br> from <src>      reset a branch to another branch
lock / unlock <p>@<br>         lock/unlock a branch (suspend non-root access)
chmod <p>@<br>/<path> mode     change rule/content mode
write <p>@<br>/<path> -- text  write an exact file (for rules only; root required)
batch {"ops":[...]}            atomic batch (combine multiple steps into one commit)

Several statements in one call:
  Separate verbs with ';' or a newline to run them in one round trip.
  '&&' runs the next one only if the previous found something.
  '||' runs the next one only if the previous found nothing - use it to fall
  back through memory layers without always paying for every layer:
      recall q --scope p@fact || recall q --scope p@record || recall q --scope p@thought
  "Found something" means hits, not exit status: a search that legitimately
  matches nothing is not an error. A real error stops the chain either way.
  Content passed after '--' is data and is never split, so a ';' inside a body
  stays in the body. Put the operator next to a space, and pass text via vars.

How to use:
  - recall something             -> recall
  - save new content             -> put
  - watch changes to a tree      -> remind <tree>
  - move memory to another node  -> sync, then fetch / push

help <verb>   show details for a verb
help --all    show all details
`)
}

// helpVerb 返回单个动词的详细说明。
func helpVerb(verb string) (string, bool) {
	switch verb {
	case "put":
		return strings.TrimSpace(`
put [<tree>] -- $TEXT
put <tree> --ref <addr>

Store content in a memory tree. If no target is given, the service chooses a default destination based on the stream declaration in SKILL.md;
when a tree path is specified, content is written as a new entry in that subtree.

Pass text separately from cmd through Termcp vars / args:
  cmd:  put notes/todo -- $TEXT
  vars: {"TEXT":"content can include spaces, newlines, and --"}

Reference mode does not copy content:
  cmd: put notes/todo --ref $1
  args: ["memories/2026-08-03/123456-local.md"]

The address returned by put is a stable file reference. Writes enter the single-writer dirty layer and are immediately searchable; policy or commit folds them into history.
`), true

	case "since":
		return strings.TrimSpace(`
since <project>@<branch>[/<tree>] <commit|empty>

List incremental changes in the tree since a commit.

Use this to resume after disconnecting: save the commit you last read, then use it after reconnecting to retrieve incremental changes,
without rereading the entire tree. If the SHA is unchanged, the result is empty; unchanged subtrees have zero cost.

Example: since proj/memories@main/ctx/team 3f2a1b9
`), true

	case "remind":
		return strings.TrimSpace(`
remind [<project>[@<branch>[/<path>]]]

Without arguments, list current watches; specify a tree path to watch it.
When the subtree on that branch changes, online clients receive a tree event. After reconnecting, watch it again,
then use the last saved commit/tree pointer to read incremental changes.
remind only registers a watch; it does not move content or assign tasks.

Example:
  remind proj/memories@main/ctx/team
  remind
`), true

	case "trees":
		return strings.TrimSpace(`
trees <project>[@<branch>]

View the current tree directory and choose paths to scope put / remind / recall.
`), true

	case "lock", "unlock":
		return strings.TrimSpace(`lock   <project>@<branch>
unlock <project>@<branch>

Lock or unlock a branch: all reads and writes by non-root credentials are denied; root is unaffected.

Locking means "pause for root to handle it": triage, transfer, archive, or reset.
Root must still be able to access it; a gate that blocks root would make the branch unusable.

Lock state is stored in zero-memcp/locks/ inside the repository; it is not part of Git history and is not visible to clone/push,
and persists across restarts. Locked branches are marked [locked] in the branch list.

Typical sequence for retiring a branch:
  lock   proj/notes@work             # the next non-root request gets 403
  ls     proj/notes@work             # triage: inspect its contents
  cp     proj/notes@work/x.md -> proj/notes@main/lessons/
  archive proj/notes@work            # finish: preserve content and move the ref to the archive namespace
  # or keep using it: unlock proj/notes@work
`), true

	case "chmod":
		return strings.TrimSpace(`chmod <project>[@<branch>]/<path> rule|content

Change a block's mode:

  rule     100755, injected into the system prompt each turn (principles, identity, hard constraints)
  content  100644, plain content, shown only when recalled

write and put can only write content. To promote existing content to a rule, use chmod;
it preserves the content and changes only the mode. Commit to apply the change.

Example: chmod proj/notes@main/rules/01.md rule
`), true

	case "remote-secret":
		return strings.TrimSpace(`remote-secret list
remote-secret put <name> -- $TOKEN
remote-secret delete <name>

Root manages tokens for HTTP push. Stored in system/secrets/<name> (0600); only the name is shown, never the contents.
For GitHub, use http_user = "x-access-token"; for GitLab and others, use your account name.
Plain HTTP is allowed only on loopback addresses: tokens must not be sent over the network.

Example: remote-secret put gh -- $TOKEN
`), true

	case "remote-key":
		return strings.TrimSpace(`remote-key list
remote-key put <name> -- $PRIVATE_KEY
remote-key delete <name>

Only root can pass a private key through vars/args. It is stored in system/sshkeys/<name>.pem (0600) and never echoed.
Only unencrypted OpenSSH PEM private keys are accepted. Configure the remote host_key = "SHA256:..." fingerprint.

Example: remote-key put github-deploy -- $KEY
`), true

	case "remote", "fetch", "push", "sync", "merge":
		return strings.TrimSpace(`
remote [<project>]                    list remotes declared for a project
fetch <project> [<remote>]        fetch remote history into refs/remotes/<remote>/*
merge <project> <branch> from <remote>
                                     merge fetched remote branch into the local branch
                                     fast-forward and disjoint changes merge automatically;
                                     same-path-different-content is refused as a conflict
push <project> [<remote>] <branch>   push one branch (built-in SSH transport; no batches)
sync <project> [<remote>]             report ahead/behind/diverged state
remote-key list | put | delete       root manages SSH private keys

Configure in TOML:

  [[remote]]
  project    = "proj/memories"
  name       = "github"
  url        = "git@github.com:org/memories.git"
  push       = ["main"]             # empty = push disabled
  fetch_only = false                # true = read-only remote
  ssh_key    = "github-deploy"     # name of the root SSH key for push
  host_key   = "SHA256:..."         # must be pinned; unknown SSH host keys are not trusted

For HTTP remotes (HTTPS), use a token:

  [[remote]]
  project    = "proj/memories"
  name       = "github"
  url        = "https://github.com/org/memories.git"
  push       = ["main"]
  http_user  = "x-access-token"     # GitHub convention
  http_token = "gh"                 # name stored by remote-secret, not the token itself

Four rules:
  - no automatic push: writes stay local; push must be called explicitly
  - no implicit commit: push is rejected with a count when there are uncommitted changes
  - no force push: +refspec is always rejected (memory history is append-only)
  - conflicts are not silent: preserve both sides and report divergence; do not auto-merge/rebase

fetch / push / sync use the built-in transports and do not invoke system git:
SSH uses Go SSH, HTTP(S) uses smart HTTP, and local bare repository paths access the object store directly.
All three transports share the same negotiation, pack generation, and fast-forward checks.
Credentials exist only in the server-side key store (remote-key / remote-secret); they are not stored in TOML or forwarded.
fetch prefers protocol v2 (ls-refs + fetch), falls back to v0 when unsupported, and includes local branch tips in have negotiation.
gc uses the built-in packer to write pack + .idx v2, also without system git.

Example:
  sync proj/memories github
  push proj/memories github main
  fetch proj/memories github
`), true

	case "recall":
		return strings.TrimSpace(`
recall <text>

Semantic search. Returns addresses, scores, and excerpts of about 240 characters around matching lines.

Why return excerpts instead of bare addresses: "addr (score)" does not show what an entry is about,
so each entry must be read individually; after reading three or four entries, users switch to grep.
not because they do not want memory tools, but because each answer requires several extra round trips. Excerpts reduce N reads to zero.

Example: recall why the port is 1729
`), true

	case "read":
		return strings.TrimSpace(`
read <addr> [<addr> ...]

Read full entries. Multiple entries can be read at once to reduce round trips.

An address is the relative path returned by recall / log / ls, e.g. 2026-10-28/023000-laptop-a1b2c3d4.md.

Directories with recall: off cannot be read; if the declaration says "do not surface this," read must not return the full text,
or the declaration is meaningless.

Example: read 2026-10-28/023000-laptop-a1b2c3d4.md
`), true

	case "grep":
		return strings.TrimSpace(`
grep <pattern> [--scope <tree>]

Search full text with a regular expression and surrounding lines.

BM25 is not good at pure symbols or proper nouns (QQ numbers, IPs, function names, variable names),
where grep is much more accurate than recall. This is the main reason models switch to bash and rg:
provide native grep and there is no need to switch tools.

--scope narrows the search; use project, project@branch, or project@branch/subtree:
As the memory tree grows, an unscoped search can return matches from projects unrelated to the current task.

Example: grep 1729
    grep 1729 --scope proj/memories
    grep func.*Server --scope proj/notes@main/rules
`), true

	case "log":
		return strings.TrimSpace(`
log [N]

Show dates, topics, and excerpts for the most recent N entries in reverse chronological order. Defaults to 5.

Use this for "what have I been doing lately?" or "where did I leave off?" instead of listing directories.

Example: log
    log 20
`), true

	case "ls":
		return strings.TrimSpace(`
ls [date] [--scope <tree>]

List entries, optionally narrowed to a day and/or a subtree.
Without a date, lists the latest day with data.

Example: ls
    ls 2026-10-28
    ls 2026-11-05 --scope BlackEggAI/zero@main.record:review
`), true

	case "status":
		return strings.TrimSpace(`
status

Index size: docs (document count) and terms (term count).

Use this to check whether the index is active and its size is reasonable. Returns no content.
`), true

	case "help":
		return strings.TrimSpace(`
help [<verb>] [--all]

no arguments: overview (same as an empty call)
with a verb: detailed help for that verb
--all: all details

Example: help
    help put
    help --all
`), true

	case "commit":
		return strings.TrimSpace(`
commit [<project>[@<branch>]] [-m <msg>]

Fold the dirty layer into a Git commit and advance the branch ref.

put only queues the write: it writes content to the directory-level dirty layer and returns; history has not advanced yet.
Folding is triggered by an explicit commit or by the background [commit] policy (count/time).

With no arguments, commit all projects; with a project/branch, commit only that branch.

Example: commit
    commit proj/memories@main
    commit proj/memories@main -m "organize port notes"
`), true

	case "diff":
		return strings.TrimSpace(`
diff <project>@<branch>
diff <project>@<branch> -> <project>@<branch>

Branch comparison: list path differences between the two sides.

The single-branch form lists changes relative to its parent commit; the arrow form compares two branches.

Branch comparison entry point: use diff audited -> raw to extract unreviewed content.

Example: diff proj/memories@main
    diff proj/memories@reviewed -> proj/memories@raw
`), true

	case "branch":
		return strings.TrimSpace(`
branch
branch <project>
branch <project> <new> [from <src>]

no arguments: list branches for all projects
with a project: list its branches
with a new branch name: fork a branch from src (defaults to the current branch)

Forking is a zero-byte operation: only a ref is written; no objects are copied.

Example: branch
    branch proj/memories
    branch proj/memories review from main
`), true

	case "tag":
		return strings.TrimSpace(`
tag <project> [<name> [<ref>]]
tag -d <project> <name>

Lightweight tag: assign a stable name to a commit.

Without a name, list all tags for the project.
-d deletes the tag but not its content; content is on the branch, and a tag is only a pointer.

Example: tag proj/memories
    tag proj/memories v1 main
    tag -d proj/memories v1
`), true

	case "cp", "mv":
		return strings.TrimSpace(`
cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]

Copy (cp) or move (mv) entries across branches.

This transfer tool is for auditing and curation: move reviewed content from another branch into main,
without reading and rewriting it (which would lose the original file timestamp and mode).

mv = cp + delete; both steps complete within the same commit boundary.

Example: cp proj/memories@work/x.md -> proj/memories@main/lessons/
    mv proj/notes@work/x.md -> proj/notes@main/lessons/
`), true

	case "delete":
		return strings.TrimSpace(`
delete <project>@<branch>/<path> [<path> ...]

Delete entries from a specified branch. Multiple entries can be deleted at once.

Deletions also enter the dirty layer; commit to remove them from history (they will be absent from the next commit).

Example: delete proj/memories@main/2026-10-28/old.md
`), true

	case "archive":
		return strings.TrimSpace(`
archive <project>@<branch> [-> <project>@<as>]

Archive a branch by moving its ref into refs/archived/; all content is preserved.

Archiving is not deletion: objects and history remain; only the branch is removed from the branch list,
and it no longer appears in the default refs for clone/push.

Typical use: when a task branch is complete, it should no longer be treated as an active line of development.

Example: archive proj/notes@work
    archive proj/notes@work -> proj/notes@work-2026-10
`), true

	case "reset":
		return strings.TrimSpace(`
reset <project>@<branch> from <project>@<src>

Reset a branch to the current position of another branch.

This overwrites history and can only be done explicitly by root; push never does this.
Reset clears the dirty layer on the destination branch: resetting discards all uncommitted state.

Example: reset proj/memories@main from proj/memories@import
`), true

	case "write":
		return strings.TrimSpace(`
write <project>[@<branch>]/<path> -- <content>

Write content to a specified path; you choose the filename and location.

Unlike put, which writes an "entry" (automatic timestamp filename + frontmatter),
write says "this path should contain exactly this content." It exists for rule files:
SKILL.md frontmatter controls injection and indexing, which cannot be expressed with put's automatic naming.

Root only: anyone who can edit SKILL.md can change who sees what, which is more dangerous than changing content itself.

Pass content through vars/args as well:
  cmd:  write proj/notes@main/SKILL.md -- $BODY
  vars: {"BODY": "---\nname: me\n---\n"}

Example: write proj/notes@main/rules/01.md -- $TEXT
`), true

	case "batch":
		return strings.TrimSpace(`
batch {"ops":[{...}, ...]}

Combine multiple write operations into one commit.

Why: when transferring a batch of entries,
executing them individually creates many intermediate commits, fragments history, and may leave partial state if an operation fails.
batch either succeeds completely or makes no changes.

Supported operations: put / cp / mv / delete / tag / commit.

Field names are action / target, not op / addr:
  {"action":"put","target":"proj@main","text":"..."}
  {"action":"delete","target":"proj@main/x.md"}
  {"action":"cp","from":"proj@main/a.md","to":"proj@main/b.md"}
  {"action":"mv","from":"proj@main/a.md","to":"proj@main/b.md"}
  {"action":"tag","project":"proj","name":"v1","ref":"main"}
  {"action":"commit","target":"proj@main","message":"..."}

Example: batch {"ops":[{"action":"delete","target":"proj@main/x.md"},{"action":"commit","target":"proj@main"}]}
`), true

	case "config":
		return strings.TrimSpace(`
config show
config path
config put <TOML>

View or update the service config file (root only).

config show displays the currently loaded effective config.
config put atomically overwrites the config file and triggers a reload (large TOML values can be passed through vars).

Example: config show
`), true

	case "reload":
		return strings.TrimSpace(`
reload

Rescan all projects and rebuild the inverted index (root only).

After directly modifying a bare repository (for example, running git commit manually or pushing with an external tool),
use this verb to force a refresh of in-memory state without restarting the process.
`), true

	case "whoami":
		return strings.TrimSpace(`
whoami

Show the caller's identity, capabilities, and mount scope.

For introspection and debugging: check this when investigating "why can't I write?"

Output has three lines:
  User       credential subject (the sub when the token was signed)
  Privilege  root or regular user
  Groups     groups (shown only for non-root)

This describes "who I am," not "what I can do":
read/write access to a specific project and branch depends on the intersection of the mount table and TOML config,
check status / projects for that, not whoami.
`), true

	case "users":
		return strings.TrimSpace(`
users

List users declared in the config and an access-control summary (root only).
`), true

	}
	return "", false
}

// helpAll 把所有动词的细节一次给全。
func helpAll() string {
	var b strings.Builder
	b.WriteString(helpOverview())
	b.WriteString("\n\n")
	for _, v := range []string{"recall", "read", "grep", "log", "ls", "put", "write", "remind", "trees", "since", "status", "chmod", "lock", "unlock", "remote-key", "remote-secret", "remote", "fetch", "push", "sync", "merge", "commit", "diff", "branch", "tag", "cp", "mv", "delete", "archive", "reset", "batch", "config", "reload", "whoami", "users", "help"} {
		if d, ok := helpVerb(v); ok {
			b.WriteString("\n-- " + v + " ")
			b.WriteString(strings.Repeat("-", max(0, 60-len(v))))
			b.WriteString("\n\n")
			b.WriteString(d)
			b.WriteString("\n")
		}
	}
	return b.String()
}
