package server

// 动词帮助页：每条动词的 Detail 常量。
// 共用页面的动词（remote/fetch/push/sync/merge 等）共用一个常量。

const detail_put = `put [<tree>] [--session <id>] $TEXT

Store content in a memory tree. If no target is given, the service chooses a default destination based on the stream declaration in SKILL.md;
when a tree path is specified, content is written as a new entry in that subtree.

Pass text separately from cmd through Termcp vars / args:
  cmd:  put notes/todo $TEXT
  vars: {"TEXT":"content can include spaces, newlines, and --"}

Reference mode does not copy content:
  cmd: ref notes/todo $1
  args: ["memories/2026-08-03/123456-local.md"]

The address returned by put is a stable file reference. Writes enter the single-writer dirty layer and are immediately searchable; policy or commit folds them into history.`

const detail_ref = `
ref <tree> <addr>

Add a reference to an existing memory entry without copying its content.
The new entry records the address instead of the bytes, so the source can be
committed, archived or expired without duplicating it.

cmd:  ref notes/todo $1
args: ["memories/2026-08-03/123456-local.md"]

Write permission on <tree> and read permission on <addr> are both required.
`

const detail_since = `
since <project>@<branch>[/<tree>] <commit|empty>

List incremental changes in the tree since a commit.

Use this to resume after disconnecting: save the commit you last read, then use it after reconnecting to retrieve incremental changes,
without rereading the entire tree. If the SHA is unchanged, the result is empty; unchanged subtrees have zero cost.

Example: since proj/memories@main/ctx/team 3f2a1b9
`

const detail_remind = `
remind [<project>[@<branch>[/<path>]]]

Without arguments, list current watches; specify a tree path to watch it.
remind only records interest plus the current head as a baseline. It does not
push anything: this service speaks MCP only. Use status to see whether a
watched tree moved, and since <tree> <sha> to fetch the actual changes.

Example:
  remind proj/memories@main/ctx/team
  remind
`

const detail_trees = `
trees <project>[@<branch>]

View the current tree directory and choose paths to scope put / remind / recall.
`

const detail_lock = `lock   <project>@<branch>
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
`

const detail_chmod = `chmod <project>[@<branch>]/<path> rule|content

Change a block's mode:

  rule     100755, injected into the system prompt each turn (principles, identity, hard constraints)
  content  100644, plain content, shown only when recalled

write and put can only write content. To promote existing content to a rule, use chmod;
it preserves the content and changes only the mode. Commit to apply the change.

Example: chmod proj/notes@main/rules/01.md rule
`

const detail_remote_secret = `remote-secret list
remote-secret put --name <name> $TOKEN
remote-secret delete <name>

Root manages tokens for HTTP push. Stored in system/secrets/<name> (0600); only the name is shown, never the contents.
For GitHub, use http_user = "x-access-token"; for GitLab and others, use your account name.
Plain HTTP is allowed only on loopback addresses: tokens must not be sent over the network.

Example: remote-secret put gh -- $TOKEN
`

const detail_remote_key = `remote-key list
remote-key put --name <name> $PRIVATE_KEY
remote-key delete <name>

Only root can pass a private key through vars/args. It is stored in system/sshkeys/<name>.pem (0600) and never echoed.
Only unencrypted OpenSSH PEM private keys are accepted. Configure the remote host_key = "SHA256:..." fingerprint.

Example: remote-key put --name github-deploy $KEY
`

const detail_remote = `
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
`

const detail_recall = `
recall <text>

Semantic search. Returns addresses, scores, and excerpts of about 240 characters around matching lines.

Why return excerpts instead of bare addresses: "addr (score)" does not show what an entry is about,
so each entry must be read individually; after reading three or four entries, users switch to grep.
not because they do not want memory tools, but because each answer requires several extra round trips. Excerpts reduce N reads to zero.

Example: recall why the port is 1729
`

const detail_read = `
read <addr> [<addr> ...]

Read full entries. Multiple entries can be read at once to reduce round trips.

An address is the relative path returned by recall / log / ls, e.g. 2026-10-28/023000-laptop-a1b2c3d4.md.

Directories with recall: off cannot be read; if the declaration says "do not surface this," read must not return the full text,
or the declaration is meaningless.

Example: read 2026-10-28/023000-laptop-a1b2c3d4.md
`

const detail_grep = `
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
`

const detail_log = `
log [N]

Show dates, topics, and excerpts for the most recent N entries in reverse chronological order. Defaults to 5.

Use this for "what have I been doing lately?" or "where did I leave off?" instead of listing directories.

Example: log
    log 20
`

const detail_ls = `
ls [date] [--scope <tree>]

List entries, optionally narrowed to a day and/or a subtree.
Without a date, lists the latest day with data.

Example: ls
    ls 2026-10-28
    ls 2026-11-05 --scope BlackEggAI/zero@main.record:review
`

const detail_status = `
status

Index size: docs (document count) and terms (term count).

Use this to check whether the index is active and its size is reasonable. Returns no content.
`

const detail_help = `
help [<verb>] [--all]

no arguments: overview (same as an empty call)
with a verb: detailed help for that verb
--all: all details

Example: help
    help put
    help --all
`

const detail_commit = `
commit [<project>[@<branch>]] [-m <msg>]

Fold the dirty layer into a Git commit and advance the branch ref.

put only queues the write: it writes content to the directory-level dirty layer and returns; history has not advanced yet.
Folding is triggered by an explicit commit or by the background [commit] policy (count/time).

With no arguments, commit all projects; with a project/branch, commit only that branch.

Example: commit
    commit proj/memories@main
    commit proj/memories@main -m "organize port notes"
`

const detail_diff = `
diff <project>@<branch>
diff <project>@<branch> --to <project>@<branch>

Branch comparison: list path differences between the two sides.

The single-branch form lists changes relative to its parent commit; the arrow form compares two branches.

Branch comparison entry point: use diff audited -> raw to extract unreviewed content.

Example: diff proj/memories@main
    diff proj/memories@reviewed -> proj/memories@raw
`

const detail_branch = `
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
`

const detail_tag = `
tag <project> [<name> [<ref>]]
tag -d <project> <name>

Lightweight tag: assign a stable name to a commit.

Without a name, list all tags for the project.
-d deletes the tag but not its content; content is on the branch, and a tag is only a pointer.

Example: tag proj/memories
    tag proj/memories v1 main
    tag -d proj/memories v1
`

const detail_cp = `
cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]

Copy (cp) or move (mv) entries across branches.

This transfer tool is for auditing and curation: move reviewed content from another branch into main,
without reading and rewriting it (which would lose the original file timestamp and mode).

mv = cp + delete; both steps complete within the same commit boundary.

Example: cp proj/memories@work/x.md -> proj/memories@main/lessons/
    mv proj/notes@work/x.md -> proj/notes@main/lessons/
`

const detail_delete = `
delete <project>@<branch>/<path> [<path> ...]

Delete entries from a specified branch. Multiple entries can be deleted at once.

Deletions also enter the dirty layer; commit to remove them from history (they will be absent from the next commit).

Example: delete proj/memories@main/2026-10-28/old.md
`

const detail_archive = `
archive <project>@<branch> [-> <project>@<as>]

Archive a branch by moving its ref into refs/archived/; all content is preserved.

Archiving is not deletion: objects and history remain; only the branch is removed from the branch list,
and it no longer appears in the default refs for clone/push.

Typical use: when a task branch is complete, it should no longer be treated as an active line of development.

Example: archive proj/notes@work
    archive proj/notes@work -> proj/notes@work-2026-10
`

const detail_reset = `
reset <project>@<branch> from <project>@<src>

Reset a branch to the current position of another branch.

This overwrites history and can only be done explicitly by root; push never does this.
Reset clears the dirty layer on the destination branch: resetting discards all uncommitted state.

Example: reset proj/memories@main from proj/memories@import
`

const detail_write = `
write <project>[@<branch>]/<path> -- <content>

Write content to a specified path; you choose the filename and location.

Unlike put, which writes an "entry" (automatic timestamp filename + frontmatter),
write says "this path should contain exactly this content." It exists for rule files:
SKILL.md frontmatter controls injection and indexing, which cannot be expressed with put's automatic naming.

Root only: anyone who can edit SKILL.md can change who sees what, which is more dangerous than changing content itself.

Pass content through vars/args as well:
  cmd:  write proj/notes@main/SKILL.md -- $BODY
  vars: {"BODY": "---\nname: me\n---\n"}

Example: write proj/notes@main/rules/01.md $TEXT
`

const detail_config = `
config show
config path
config put <TOML>

View or update the service config file (root only).

config show displays the currently loaded effective config.
config put atomically overwrites the config file and triggers a reload (large TOML values can be passed through vars).

Example: config show
`

const detail_reload = `
reload

Rescan all projects and rebuild the inverted index (root only).

After directly modifying a bare repository (for example, running git commit manually or pushing with an external tool),
use this verb to force a refresh of in-memory state without restarting the process.
`

const detail_whoami = `
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
`

const detail_users = `
users

List users declared in the config and an access-control summary (root only).
`
