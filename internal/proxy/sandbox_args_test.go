package proxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestMacOSExecArgs(t *testing.T) {
	enforced := []string{"--skip-git-repo-check", "--sandbox", "danger-full-access"}
	with := func(prefix, tail []string) []string {
		result := append(append([]string{}, prefix...), enforced...)
		return append(result, tail...)
	}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"exec", []string{"exec"}, with([]string{"exec"}, nil)},
		{"alias and experimental json", []string{"e", "--experimental-json", "prompt"}, with([]string{"e"}, []string{"--experimental-json", "prompt"})},
		{"unknown options", []string{"exec", "--future-option", "value", "--another=--sandbox", "-x", "prompt"}, with([]string{"exec"}, []string{"--future-option", "value", "--another=--sandbox", "-x", "prompt"})},
		{"unknown top-level option", []string{"--future-option", "exec", "prompt"}, with([]string{"--future-option", "exec"}, []string{"prompt"})},
		{"existing skip flag", []string{"exec", "--skip-git-repo-check", "prompt"}, []string{"exec", "--sandbox", "danger-full-access", "--skip-git-repo-check", "prompt"}},
		{"separate sandbox", []string{"exec", "--sandbox", "read-only", "--json", "prompt"}, []string{"exec", "--skip-git-repo-check", "--sandbox", "danger-full-access", "--json", "prompt"}},
		{"equal sandbox", []string{"exec", "--sandbox=workspace-write", "prompt"}, []string{"exec", "--skip-git-repo-check", "--sandbox", "danger-full-access", "prompt"}},
		{"short sandbox", []string{"exec", "-s", "read-only", "prompt"}, []string{"exec", "--skip-git-repo-check", "--sandbox", "danger-full-access", "prompt"}},
		{"attached short sandbox", []string{"exec", "-sread-only", "prompt"}, []string{"exec", "--skip-git-repo-check", "--sandbox", "danger-full-access", "prompt"}},
		{"attached equal short sandbox", []string{"exec", "-s=read-only", "prompt"}, []string{"exec", "--skip-git-repo-check", "--sandbox", "danger-full-access", "prompt"}},
		{"both policy options exist", []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "prompt"}, []string{"exec", "--sandbox", "danger-full-access", "--skip-git-repo-check", "prompt"}},
		{"sandbox before command", []string{"--sandbox", "read-only", "--model", "m", "exec", "prompt"}, []string{"--sandbox", "danger-full-access", "--model", "m", "exec", "--skip-git-repo-check", "prompt"}},
		{"policy options after prompt", []string{"exec", "prompt", "--sandbox", "read-only", "--skip-git-repo-check"}, []string{"exec", "prompt", "--sandbox", "danger-full-access", "--skip-git-repo-check"}},
		{"command name used as value", []string{"--model", "exec", "--config", "e", "exec", "prompt"}, with([]string{"--model", "exec", "--config", "e", "exec"}, []string{"prompt"})},
		{"command after top prompt", []string{"initial prompt", "exec", "prompt"}, with([]string{"initial prompt", "exec"}, []string{"prompt"})},
		{"values mentioning sandbox preserved", []string{"exec", "--model=--sandbox", "-m--sandbox", "--config", "key=--sandbox", "-o", "file--sandbox", "prompt --sandbox read-only"}, with([]string{"exec"}, []string{"--model=--sandbox", "-m--sandbox", "--config", "key=--sandbox", "-o", "file--sandbox", "prompt --sandbox read-only"})},
		{"literal policy options after terminator", []string{"exec", "--", "--sandbox", "read-only", "--skip-git-repo-check"}, with([]string{"exec"}, []string{"--", "--sandbox", "read-only", "--skip-git-repo-check"})},
		{"resume external options", []string{"exec", "resume", "--last", "--skip-git-repo-check", "--experimental-json", "--future-resume-option"}, []string{"exec", "--sandbox", "danger-full-access", "resume", "--last", "--skip-git-repo-check", "--experimental-json", "--future-resume-option"}},
		{"fork external options", []string{"exec", "fork", "session-id", "--future-fork-option", "prompt"}, with([]string{"exec"}, []string{"fork", "session-id", "--future-fork-option", "prompt"})},
		{"review option values", []string{"exec", "review", "--base=--sandbox", "--title", "title --sandbox read-only", "prompt"}, with([]string{"exec"}, []string{"review", "--base=--sandbox", "--title", "title --sandbox read-only", "prompt"})},
		{"approval options retained", []string{"--approve-for-me", "exec", "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox"}, with([]string{"--approve-for-me", "exec"}, []string{"--approve-for-me", "--dangerously-bypass-approvals-and-sandbox"})},
		{"image boundary retained", []string{"exec", "--image", "a.png", "b.png", "--sandbox=read-only", "prompt"}, []string{"exec", "--skip-git-repo-check", "--image", "a.png", "b.png", "--sandbox", "danger-full-access", "prompt"}},
		{"image value looks like command", []string{"--image", "exec", "e", "--model", "m", "exec", "prompt"}, with([]string{"--image", "exec", "e", "--model", "m", "exec"}, []string{"prompt"})},
		{"resume single image value", []string{"exec", "resume", "--image", "image.png", "session-id", "prompt"}, with([]string{"exec"}, []string{"resume", "--image", "image.png", "session-id", "prompt"})},
		{"help tail retained", []string{"exec", "--help", "--future-option"}, with([]string{"exec"}, []string{"--help", "--future-option"})},
		{"help child retained", []string{"exec", "help", "resume"}, with([]string{"exec"}, []string{"help", "resume"})},
		{"missing unmanaged value passed through", []string{"exec", "--model"}, with([]string{"exec"}, []string{"--model"})},
		{"invalid unmanaged value passed through", []string{"exec", "--experimental-json=true", "--help=true"}, with([]string{"exec"}, []string{"--experimental-json=true", "--help=true"})},
		{"empty prompt", []string{"exec", ""}, with([]string{"exec"}, []string{""})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			got, err := macOSExecArgs(tt.args)
			if err != nil {
				t.Fatalf("macOSExecArgs(%q): %v", tt.args, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("macOSExecArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Errorf("argument rewrite mutated its input: got %q, originally %q", tt.args, original)
			}
			got[0] = "changed"
			if !reflect.DeepEqual(tt.args, original) {
				t.Errorf("argument rewrite aliases its input")
			}
		})
	}
}

func TestMacOSExecArgsRequiresExec(t *testing.T) {
	for _, args := range [][]string{nil, {"app-server"}, {"--", "exec"}, {"--image", "image.png", "exec"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := macOSExecArgs(args); err == nil {
				t.Errorf("macOSExecArgs(%q) unexpectedly succeeded", args)
			}
		})
	}
}
