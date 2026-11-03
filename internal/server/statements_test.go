package server

import (
	"reflect"
	"strings"
	"testing"
)

// Statement splitting has to hold two opposite cases at once:
// a ';' inside a body is data, and a ';' after a body is a separator.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single statement",
			in:   "recall database",
			want: []string{"recall database"},
		},
		{
			name: "semicolon separates",
			in:   "recall a; recall b",
			want: []string{"recall a", "recall b"},
		},
		{
			name: "newline separates",
			in:   "recall a\nrecall b",
			want: []string{"recall a", "recall b"},
		},
		{
			name: "empty pieces are dropped",
			in:   "recall a;;; recall b;",
			want: []string{"recall a", "recall b"},
		},
		{
			// The whole point of the payload boundary: user content is not syntax.
			name: "semicolon inside an inline body is data",
			in:   "put notes -- a; b",
			want: []string{"put notes -- a; b"},
		},
		{
			name: "newline inside an inline body is data",
			in:   "write proj@zero/x.md -- line one\nline two",
			want: []string{"write proj@zero/x.md -- line one\nline two"},
		},
		{
			// A flag that merely starts with "--" is not the payload separator.
			name: "scope flag is not a payload separator",
			in:   "recall database --scope proj@fact; recall other",
			want: []string{"recall database --scope proj@fact", "recall other"},
		},
		{
			name: "semicolon inside an address is not a separator",
			in:   "read proj@zero/a--b.md; status",
			want: []string{"read proj@zero/a--b.md", "status"},
		},
		{
			name: "trailing whitespace is trimmed",
			in:   "  recall a  ;  recall b  ",
			want: []string{"recall a", "recall b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitStatements(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Operators are recorded, not just stripped.
func TestSplitStatementsWithOps(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Statement
	}{
		{
			name: "sequential",
			in:   "recall a; recall b",
			want: []Statement{{Cmd: "recall a", Op: ""}, {Cmd: "recall b", Op: ";"}},
		},
		{
			name: "and",
			in:   "recall a && recall b",
			want: []Statement{{Cmd: "recall a", Op: ""}, {Cmd: "recall b", Op: "&&"}},
		},
		{
			name: "or chain",
			in:   "recall a || recall b || recall c",
			want: []Statement{{Cmd: "recall a", Op: ""}, {Cmd: "recall b", Op: "||"}, {Cmd: "recall c", Op: "||"}},
		},
		{
			name: "mixed, left to right",
			in:   "recall a && recall b || recall c",
			want: []Statement{{Cmd: "recall a", Op: ""}, {Cmd: "recall b", Op: "&&"}, {Cmd: "recall c", Op: "||"}},
		},
		{
			name: "single statement has no operator",
			in:   "recall a",
			want: []Statement{{Cmd: "recall a", Op: ""}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatementsWithOps(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitStatementsWithOps(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}
}

// A single '&' or '|' is not an operator and must not be swallowed.
//
// grep a|b and addresses containing a dash-adjacent character are both legal
// DSL, so only the doubled forms count.
func TestSplitStatementsSinglePipeIsNotAnOperator(t *testing.T) {
	got := splitStatementsWithOps("grep a|b")
	if len(got) != 1 || got[0].Cmd != "grep a|b" {
		t.Fatalf("a lone '|' must stay in the statement, got %+v", got)
	}
	got = splitStatementsWithOps("grep x&y")
	if len(got) != 1 || got[0].Cmd != "grep x&y" {
		t.Fatalf("a lone '&' must stay in the statement, got %+v", got)
	}
}

// A value passed through vars is one atom, so parsing resumes after it.
func TestSplitStatementsResumesAfterInjectedPayload(t *testing.T) {
	body := "line one; line two\nand a newline"
	cmd, err := expandArgs("put notes -- $B ; commit notes@zero -m done", map[string]string{"B": body}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := splitStatements(cmd)
	if len(got) != 2 {
		t.Fatalf("expected 2 statements, got %d: %q", len(got), got)
	}
	if verbOf(got[0]) != "put" {
		t.Fatalf("first statement should be the put, got %q", got[0])
	}
	if verbOf(got[1]) != "commit" {
		t.Fatalf("second statement should be the commit, got %q", got[1])
	}
	if restored := restorePayload(got[0]); !strings.Contains(restored, body) {
		t.Fatalf("body was mangled by splitting: %q", restored)
	}
}

func TestGluedReferenceIsRefusedWithExplanation(t *testing.T) {
	_, err := expandArgs("put notes -- $B; commit notes@zero -m done", map[string]string{"B": "x"}, nil)
	if err == nil {
		t.Fatal("a reference glued to ';' must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "glued to a statement separator") {
		t.Fatalf("error should name the real cause, got %q", msg)
	}
	if strings.Contains(msg, "no such name") {
		t.Fatalf("error should not blame the vars map, got %q", msg)
	}
}

func TestVerbOf(t *testing.T) {
	cases := map[string]string{
		"recall database":       "recall",
		"  status  ":            "status",
		"":                      "",
		"put notes -- a; b":     "put",
		"commit proj@zero -m x": "commit",
	}
	for in, want := range cases {
		if got := verbOf(in); got != want {
			t.Errorf("verbOf(%q) = %q, want %q", in, got, want)
		}
	}
}
