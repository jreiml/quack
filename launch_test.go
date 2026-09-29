package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/creack/pty"
)

func TestNamedAgentLaunch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(root, "arguments")
	t.Setenv("QUACK_LAUNCH_ARGS", output)
	for _, agent := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(root, agent), []byte("#!/bin/sh\nif [ \"$1\" = app-server ]; then : > \"${3#unix://}\"; exec sleep 600; fi\nprintf '%s\\0' \"$@\" > \"$QUACK_LAUNCH_ARGS\"\nexec /bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	settings := "{\"name\":\"Ada Lovelace\"}\n"
	for _, tc := range []struct {
		title string
		args  []string
		name  string
		want  []string
	}{
		{"claude", []string{"claude", "-n", "t-named-claude", "--model", "opus", "--settings", settings}, "t-named-claude", []string{"--dangerously-skip-permissions", "--name", "t-named-claude", "--model", "opus", "--settings", settings}},
		{"codex", []string{"codex", "--model", "gpt-6-astra", "--name=t-named-codex", "resume", "Ada's work"}, "t-named-codex", []string{"--no-daemon", "--model", "gpt-6-astra", "resume", "Ada's work"}},
		{"codex-native", []string{"codex", "--model", "gpt-6-astra", "--name=t-native-codex"}, "t-native-codex", []string{"--remote", "unix://" + filepath.Join(socketDir(), "codex-*.sock"), "--model", "gpt-6-astra"}},
		{"separator", []string{"claude", "--name", "t-named-separator", "--", "-n", "inner-name", "--settings", settings}, "t-named-separator", []string{"--dangerously-skip-permissions", "--name", "t-named-separator", "-n", "inner-name", "--settings", settings}},
		{"agent-only-name", []string{"codex", "--", "--name", "inner-name"}, "", []string{"--no-daemon", "--name", "inner-name"}},
	} {
		t.Run(tc.title, func(t *testing.T) {
			if tc.title == "codex-native" {
				t.Setenv("QUACK_CODEX_NATIVE", "1")
			}
			if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			name := quack(t, tc.args...)
			s := server{name}
			defer s.run("kill-server")
			if tc.name != "" && name != tc.name {
				t.Fatalf("session name %q, want %q", name, tc.name)
			}
			if tc.name == "" && name == "inner-name" {
				t.Fatal("consumed agent-only name")
			}
			if got := s.must("show-environment", "-t", "=main", "QUACK_SESSION"); got != "QUACK_SESSION="+name {
				t.Fatal(got)
			}
			eventually(t, "agent arguments", func() bool { _, err := os.Stat(output); return err == nil })
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			if len(got) > 1 && len(tc.want) > 1 && got[0] == "--remote" && tc.want[0] == "--remote" {
				if ok, err := filepath.Match(tc.want[1], got[1]); err != nil || !ok {
					t.Fatalf("app server %q, want %q", got[1], tc.want[1])
				}
				got[1] = tc.want[1]
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("arguments %q, want %q", got, tc.want)
			}
		})
	}
	out, err := exec.Command(bin, "new", "-n", "t-no-tty").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no terminal here") {
		t.Fatalf("%s %v", out, err)
	}
	if (server{"t-no-tty"}).alive() {
		t.Fatal("started a session without a terminal")
	}
	for _, args := range [][]string{{"claude", "-n"}, {"codex", "--name="}, {"codex", "-n", "--"}} {
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "needs a session name") {
			t.Fatalf("%q: %s %v", args, out, err)
		}
	}
}

func TestAgentPassthrough(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(root, "arguments")
	t.Setenv("QUACK_LAUNCH_ARGS", output)
	for _, agent := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(root, agent), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$QUACK_LAUNCH_ARGS\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		title string
		tty   bool
		env   string
		args  []string
		want  []string
	}{
		{"no-tty-claude", false, "", []string{"claude", "-n", "t-pass", "fix it"}, []string{"--dangerously-skip-permissions", "--name", "t-pass", "fix it"}},
		{"no-tty-codex", false, "", []string{"codex", "-n", "t-pass", "resume", "--last"}, []string{"resume", "--last"}},
		{"inside-session", true, "t-outer", []string{"codex", "resume", "--last"}, []string{"resume", "--last"}},
		{"print", true, "", []string{"claude", "--output-format=json", "-p", "hi"}, []string{"--dangerously-skip-permissions", "--output-format=json", "-p", "hi"}},
		{"claude-command", true, "", []string{"claude", "mcp", "list"}, []string{"--dangerously-skip-permissions", "mcp", "list"}},
		{"codex-exec", true, "", []string{"codex", "exec", "hi"}, []string{"exec", "hi"}},
		{"codex-version", true, "", []string{"codex", "--version"}, []string{"--version"}},
	} {
		t.Run(tc.title, func(t *testing.T) {
			t.Setenv("QUACK_SESSION", tc.env)
			if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			c := exec.Command(bin, tc.args...)
			if tc.tty {
				f, err := pty.Start(c)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				go io.Copy(io.Discard, f)
				if err := c.Wait(); err != nil {
					t.Fatal(err)
				}
			} else if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("%s %v", out, err)
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("arguments %q, want %q", got, tc.want)
			}
			if (server{"t-pass"}).alive() {
				t.Fatal("started a session")
			}
		})
	}
}
