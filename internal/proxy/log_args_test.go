package proxy

import (
	"reflect"
	"slices"
	"testing"
)

func TestLogArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no arguments", nil, []string{}},
		{"login", []string{"login", "--device-auth", "value"}, []string{"login", "--device-auth", "value"}},
		{"app server", []string{"-C", "/workspace", "app-server", "--listen", "stdio://"}, []string{"app-server"}},
		{"interactive", []string{"--model", "m", "interactive prompt"}, []string{"--model", "m", "interactive prompt"}},
		{"exec without prompt", []string{"exec"}, []string{"exec"}},
		{"exec prompt", []string{"exec", "private prompt"}, []string{"exec"}},
		{"alias", []string{"e", "--json", "private prompt"}, []string{"e", "--json"}},
		{"empty prompt", []string{"exec", ""}, []string{"exec"}},
		{"stdin marker", []string{"exec", "--json", "-"}, []string{"exec", "--json"}},
		{"option values", []string{"exec", "-m", "review", "-s", "read-only", "-o", "answer.txt", "private prompt"}, []string{"exec", "-m", "review", "-s", "read-only", "-o", "answer.txt"}},
		{"trailing option value", []string{"exec", "--model", "test-model"}, []string{"exec", "--model", "test-model"}},
		{"trailing flag", []string{"exec", "--json"}, []string{"exec", "--json"}},
		{"bypass flag", []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "private prompt"}, []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}},
		{"bypass flag after prompt", []string{"exec", "private prompt", "--dangerously-bypass-approvals-and-sandbox"}, []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}},
		{"attached values", []string{"exec", "-mtest-model", "--config=key=value", "-oanswer.txt", "private prompt"}, []string{"exec", "-mtest-model", "--config=key=value", "-oanswer.txt"}},
		{"options after prompt", []string{"exec", "private prompt", "--json", "--color", "never"}, []string{"exec", "--json", "--color", "never"}},
		{"top-level values and prompt", []string{"--model", "exec", "initial private prompt", "exec", "--json", "private prompt"}, []string{"--model", "exec", "exec", "--json"}},
		{"prompt after terminator", []string{"exec", "--", "--model private prompt"}, []string{"exec", "--"}},
		{"multiple positional tokens", []string{"exec", "--", "--model", "private prompt"}, []string{"exec", "--"}},
		{"unknown option", []string{"exec", "--future-option", "private prompt"}, []string{"exec", "--future-option"}},
		{"unknown bare value", []string{"exec", "--future-option", "ambiguous value", "private prompt"}, []string{"exec", "--future-option"}},
		{"unknown attached value", []string{"exec", "--future-option=value", "private prompt"}, []string{"exec", "--future-option=value"}},
		{"resume", []string{"exec", "resume", "session-id", "--json", "private prompt"}, []string{"exec", "resume", "session-id", "--json"}},
		{"resume without prompt", []string{"exec", "resume", "session-id"}, []string{"exec", "resume", "session-id"}},
		{"resume last", []string{"exec", "resume", "--last", "private prompt"}, []string{"exec", "resume", "--last"}},
		{"resume last after prompt", []string{"exec", "resume", "private prompt", "--last", "--json"}, []string{"exec", "resume", "--last", "--json"}},
		{"resume terminated session", []string{"exec", "resume", "--", "session-id", "private prompt"}, []string{"exec", "resume", "--", "session-id"}},
		{"fork", []string{"exec", "fork", "session-id", "private prompt"}, []string{"exec", "fork", "session-id"}},
		{"fork without prompt", []string{"exec", "fork", "session-id"}, []string{"exec", "fork", "session-id"}},
		{"review", []string{"exec", "review", "--base", "main", "--title", "review title", "private prompt"}, []string{"exec", "review", "--base", "main", "--title", "review title"}},
		{"help", []string{"exec", "help", "resume"}, []string{"exec", "help", "resume"}},
		{"multiple images", []string{"exec", "--image", "a.png", "b.png", "--json", "private prompt"}, []string{"exec", "--image", "a.png", "b.png", "--json"}},
		{"resume image", []string{"exec", "resume", "--image", "a.png", "session-id", "private prompt"}, []string{"exec", "resume", "--image", "a.png", "session-id"}},
		{"program value", []string{"exec", "--cyber-access-program", "standard", "private prompt"}, []string{"exec", "--cyber-access-program", "standard"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := slices.Clone(tt.args)
			got := logArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("logArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatal("log filtering modified execution arguments")
			}
			if len(got) > 0 {
				got[0] = "changed"
				if !reflect.DeepEqual(tt.args, original) {
					t.Fatal("log arguments alias execution arguments")
				}
			}
		})
	}
}

func TestLogArgsIncludesMacOSExecOptions(t *testing.T) {
	args, err := macOSExecArgs([]string{"exec", "--sandbox", "read-only", "--json", "private prompt"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "--json"}
	if got := logArgs(args); !reflect.DeepEqual(got, want) {
		t.Errorf("logged macOS args = %q, want %q", got, want)
	}
}

func TestLogArgsPreservesMacOSImageBoundary(t *testing.T) {
	for _, imageOption := range []string{"--image", "-i"} {
		t.Run(imageOption, func(t *testing.T) {
			args, err := macOSExecArgs([]string{"exec", imageOption, "a.png", "b.png", "--sandbox", "read-only", "private prompt"})
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := []string{"exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "--image=a.png", "--image=b.png", "private prompt"}
			if !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("macOS args = %q, want %q", args, wantArgs)
			}
			wantLogArgs := wantArgs[:len(wantArgs)-1]
			if got := logArgs(args); !reflect.DeepEqual(got, wantLogArgs) {
				t.Errorf("logged macOS args = %q, want %q", got, wantLogArgs)
			}
		})
	}
}
