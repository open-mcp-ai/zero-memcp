package gitremote

import "testing"

func TestParseSSHURL(t *testing.T) {
	cases := []struct {
		in   string
		user string
		host string
		port string
		path string
	}{
		{"git@github.example:org/repo.git", "git", "github.example", "22", "org/repo.git"},
		{"ssh://deploy@git.example:2222/team/repo.git", "deploy", "git.example", "2222", "team/repo.git"},
		{"ssh://git.example/team/repo.git", "git", "git.example", "22", "team/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSSHURL(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.User != tc.user || got.Host != tc.host || got.Port != tc.port || got.Path != tc.path {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestIsSSH(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"git@github.example:org/repo.git", true},
		{"ssh://git@github.example/org/repo.git", true},
		{"https://github.example/org/repo.git", false},
		{"/tmp/repo.git", false},
	} {
		if got := IsSSH(tc.in); got != tc.want {
			t.Errorf("IsSSH(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}
