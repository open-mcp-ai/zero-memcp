package server

import (
	"strings"
)

// splitStatements splits a DSL line into statements.
//
// Statements are separated by ';' or a newline, so one call can run several
// verbs. This matters for a layered memory: recall returns three hits, so
// asking fact, record and thought separately is three queries - one round trip
// beats three.
//
// THE PAYLOAD SEPARATOR IS A HARD BOUNDARY. Everything after a standalone `--`
// is data, and data is never parsed: a ';' inside a body is a semicolon, not a
// second command. That is the whole reason bodies and DSL are kept apart, and
// doing it here rather than by re-parsing the line later means user content can
// never become a command.
//
// Two shapes follow from that:
//
//	put notes -- a; b            one statement, body "a; b"
//	put notes -- $B; recall q    two statements
//
// The second works because a value from vars/args arrives as one encoded atom
// (~ZMEMARG~...~END~, base64url: no whitespace, no ';'). The atom ends the
// payload - there is nothing of the caller's inside it to split - so parsing
// resumes right after it. An inline body has no such marker, so it takes the
// rest of the line.
//
// Quotes are NOT special. zero-memcp's DSL has never had shell quoting (words
// are split on whitespace), and pretending otherwise would make `grep 'a;b'`
// look supported when the quotes would end up in the pattern. Data containing a
// separator goes through vars/args, which is the mechanism that actually works.
func splitStatements(line string) []string {
	parsed := splitStatementsWithOps(line)
	out := make([]string, 0, len(parsed))
	for _, st := range parsed {
		out = append(out, st.Cmd)
	}
	return out
}

// Statement is one command in a line, plus how it connects to the previous one.
type Statement struct {
	// Cmd is the statement with delimiters removed.
	Cmd string
	// Op is the operator that precedes this statement: "" for the first one,
	// then ";" (always), "&&" (only if the previous one found something) or
	// "||" (only if it did not).
	Op string
}

// splitStatementsWithOps splits a line and records the connecting operators.
//
// `;` runs the next statement unconditionally. `&&` and `||` are the shell's
// idea applied to this store's idea of success: a statement that ran without
// error and found something is a success, one that ran without error and found
// nothing is not. That distinction is what makes layered fallback expressible:
//
//	recall q --scope proj@fact || recall q --scope proj@record
//
// asks the second layer only when the first came back empty. Note this is NOT
// shell's `||`, which looks at exit status: a query with no hits is not an
// error here (the answer "nothing matched" is a real answer), so keying the
// fallback on errors would make it never fire.
func splitStatementsWithOps(line string) []Statement {
	var out []Statement
	var cur strings.Builder
	pendingOp := ""
	atTokenStart := func() bool {
		s := cur.String()
		if s == "" {
			return true
		}
		last := s[len(s)-1]
		return last == ' ' || last == '\t' || last == '\n'
	}
	flush := func(op string) {
		if stmt := strings.TrimSpace(cur.String()); stmt != "" {
			out = append(out, Statement{Cmd: stmt, Op: op})
		}
		cur.Reset()
	}

	for i := 0; i < len(line); i++ {
		ch := line[i]

		// A standalone "--" starts the payload. It must be a whole word: "--scope"
		// is a flag, and "--x" inside a path is part of an address.
		if ch == '-' && atTokenStart() && strings.HasPrefix(line[i:], "--") {
			after := line[i+2:]
			if after == "" || after[0] == ' ' || after[0] == '\t' || after[0] == '\n' {
				cur.WriteString("--")
				i++
				// Decide how the payload ends.
				rest := line[i+1:]
				if trimmed := strings.TrimLeft(rest, " \t"); strings.HasPrefix(trimmed, payloadPrefix) {
					// A value from vars/args: one atom, then normal parsing resumes.
					cur.WriteString(rest[:len(rest)-len(trimmed)])
					end := strings.Index(trimmed, payloadSuffix)
					if end < 0 {
						cur.WriteString(trimmed)
						break
					}
					end += len(payloadSuffix)
					cur.WriteString(trimmed[:end])
					i += len(rest) - len(trimmed) + end - 1
					continue
				}
				// An inline body: it owns the rest of the line, separators and all.
				cur.WriteString(rest)
				break
			}
		}

		if ch == '&' && strings.HasPrefix(line[i:], "&&") {
			flush(pendingOp)
			pendingOp = "&&"
			i++
			continue
		}
		if ch == '|' && strings.HasPrefix(line[i:], "||") {
			flush(pendingOp)
			pendingOp = "||"
			i++
			continue
		}
		if ch == ';' || ch == '\n' {
			flush(pendingOp)
			pendingOp = ";"
			continue
		}
		cur.WriteByte(ch)
	}
	flush(pendingOp)
	return out
}

// verbOf extracts the leading word of a statement, for port-policy checking.
func verbOf(stmt string) string {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// describeStatement renders a statement as a short, single-line label.
//
// Encoded payload atoms are replaced: a base64 body says nothing useful and can
// be long. What remains identifies the statement - the verb and its addresses -
// which is exactly what a caller needs to tell one block of a multi-statement
// reply from the next.
func describeStatement(stmt string) string {
	fields := strings.Fields(stmt)
	out := make([]string, 0, len(fields))
	elided := false
	for _, f := range fields {
		if strings.HasPrefix(f, payloadPrefix) {
			out = append(out, "…")
			elided = true
			continue
		}
		out = append(out, f)
	}
	label := "[statement] " + strings.Join(out, " ")
	if elided {
		label += " (payload passed through vars)"
	}
	const limit = 120
	if len(label) > limit {
		label = label[:limit] + "…"
	}
	return label
}
