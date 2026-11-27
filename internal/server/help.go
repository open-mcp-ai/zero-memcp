package server

import "strings"

// helpOverview 是空调用与 help 的默认输出：有什么、怎么选。
//
// 它要回答的不是"有哪些命令"，而是"我现在该用哪个"——
// 一个只列出动词名的手册，看完还是要猜。
func helpOverview() string {
	return strings.TrimSpace(`
zero-memcp: persistent memory. Writes are stored locally and remain available after restart.

put [<tree>] $TEXT           write to a tree (put --where [<tree>] probes destination without writing)
ref <tree> <addr>            add a reference to existing memory in a tree without copying its content
recall <text> [--scope tree] semantic search; searches long-term memory by default, with an optional scope
read <addr> [<addr>...]     read full text; accepts multiple addresses
grep [-i|-c|-l] <re> [--scope T] search full text (-i ignore case, -c count, -l files only)
log [N]                     timeline of recent memories
ls [date]                   list memory entries for a date
trees <project>[@<branch>]  view the tree directory
remind [<tree>]             record interest in a tree; status then reports whether it moved
since <tree> <commit|empty> retrieve tree changes since the last seen commit
status                      index size

Memory sync (remotes are declared in the config):
remote [<project>]                    list configured memory remotes
fetch <project> [<remote>]            fetch history from a remote without automatically overwriting local branches
push <project> [<remote>] <branch>   explicitly push one branch (one at a time; see README for external git push)
                                     --snapshot = orphan commit, no history; --force = allow rewrite
sync <project> [<remote>]             compare local and remote state
remote-key list | put | delete       root manages SSH private keys (for external push)
remote-secret list | put | delete    root manages HTTP tokens (for external push)

push requires an explicitly named branch; it does not commit or force-push for you.

Commands for maintaining memory structure (not needed for routine reading and writing):
commit [<p>[@<br>]] [-m msg]   explicitly fold the dirty layer into history
diff <p>@<br> [--to <p>@<br>] compare branch differences (for auditing)
branch [<p> [<new> [from s]]]  list or fork branches
tag [-d] <p> [<name> [<ref>]]  lightweight tag
cp / mv <src> --to <dst>       transfer entries across branches
delete <p>@<br>/<path>...      delete entries
archive <p>@<br>               archive a branch
reset <p>@<br> from <src>      reset a branch to another branch
lock / unlock <p>@<br>         lock/unlock a branch (suspend non-root access)
chmod <p>@<br>/<path> mode     change rule/content mode
write <p>@<br>/<path> $TEXT   write an exact file (for rules only; root required)

Several statements in one call:
  Separate verbs with ';' or a newline to run them in one round trip.
  '&&' runs the next one only if the previous found something.
  '||' runs the next one only if the previous found nothing - use it to fall
  back through memory layers without always paying for every layer:
      recall q --scope p@fact || recall q --scope p@record || recall q --scope p@thought
  "Found something" means hits, not exit status: a search that legitimately
  matches nothing is not an error. A real error stops the chain either way.
  Output pipes: '| head [N]', '| tail [N]', and '| less' slice or inspect output.
  Content ALWAYS travels through vars/args as a variable reference ($TEXT or $1):
  the protocol refuses an inline body outright. A ';' inside a value stays in
  that value, because values are spliced in and never re-read as syntax.

How to use:
  - recall something             -> recall
  - save new content             -> put
  - track whether a tree moved   -> remind <tree>, then status / since
  - move memory to another node  -> sync, then fetch / push

help <verb>   show details for a verb
help --all    show all details
`)
}

// helpVerb 返回单个动词的详细说明。
func helpVerb(verb string) (string, bool) {
	for _, op := range zeroGrammar.Ops() {
		if op.Name != verb || op.Detail == "" {
			continue
		}
		return strings.TrimSpace(op.Detail), true
	}
	return "", false
}

// helpAll 把所有动词的细节一次给全。
func helpAll() string {
	var b strings.Builder
	b.WriteString(helpOverview())
	b.WriteString("\n\n")
	for _, v := range []string{"recall", "read", "grep", "log", "ls", "put", "ref", "write", "remind", "trees", "since", "status", "chmod", "lock", "unlock", "remote-key", "remote-secret", "remote", "fetch", "push", "sync", "merge", "commit", "diff", "branch", "tag", "cp", "mv", "delete", "erase", "replace", "archive", "reset", "acl", "config", "reload", "whoami", "users", "help"} {
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
