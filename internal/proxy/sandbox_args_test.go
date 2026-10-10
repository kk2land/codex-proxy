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
		{"alias and prompt", []string{"e", "--json", "prompt"}, with([]string{"e"}, []string{"--json", "prompt"})},
		{"separate sandbox", []string{"exec", "--sandbox", "read-only", "--json", "prompt"}, with([]string{"exec"}, []string{"--json", "prompt"})},
		{"equal sandbox", []string{"exec", "--sandbox=workspace-write", "prompt"}, with([]string{"exec"}, []string{"prompt"})},
		{"short sandbox", []string{"exec", "-s", "read-only", "prompt"}, with([]string{"exec"}, []string{"prompt"})},
		{"attached short sandbox", []string{"exec", "-sread-only", "prompt"}, with([]string{"exec"}, []string{"prompt"})},
		{"attached equal short sandbox", []string{"exec", "-s=read-only", "prompt"}, with([]string{"exec"}, []string{"prompt"})},
		{"all repeated sandbox options", []string{"exec", "-sread-only", "--sandbox", "workspace-write", "--sandbox=danger-full-access", "--skip-git-repo-check", "--skip-git-repo-check"}, with([]string{"exec"}, nil)},
		{"sandbox before command", []string{"--sandbox", "read-only", "--model", "m", "exec", "-sworkspace-write", "prompt"}, with([]string{"--model", "m", "exec"}, []string{"prompt"})},
		{"short sandbox before alias", []string{"-sread-only", "e", "prompt"}, with([]string{"e"}, []string{"prompt"})},
		{"command after top prompt", []string{"initial prompt", "--sandbox=read-only", "exec", "prompt"}, with([]string{"initial prompt", "exec"}, []string{"prompt"})},
		{"command name used as value", []string{"--model", "exec", "--config", "e", "exec", "prompt"}, with([]string{"--model", "exec", "--config", "e", "exec"}, []string{"prompt"})},
		{"config sandbox preserved", []string{"-c", "sandbox_mode=read-only", "exec", "--config=sandbox_mode=workspace-write", "--sandbox=read-only"}, with([]string{"-c", "sandbox_mode=read-only", "exec"}, []string{"--config=sandbox_mode=workspace-write"})},
		{"values mentioning sandbox preserved", []string{"exec", "--model=--sandbox", "-m--sandbox", "--config", "key=--sandbox", "-o", "file--sandbox", "prompt --sandbox read-only"}, with([]string{"exec"}, []string{"--model=--sandbox", "-m--sandbox", "--config", "key=--sandbox", "-o", "file--sandbox", "prompt --sandbox read-only"})},
		{"literal sandbox after terminator", []string{"exec", "--sandbox=read-only", "--", "--sandbox", "read-only", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}, with([]string{"exec"}, []string{"--", "--sandbox", "read-only", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"})},
		{"option after prompt", []string{"exec", "prompt", "--sandbox", "read-only", "--json"}, with([]string{"exec"}, []string{"prompt", "--json"})},
		{"resume parent sandbox", []string{"exec", "--sandbox", "read-only", "resume", "--last", "--skip-git-repo-check", "--json"}, with([]string{"exec"}, []string{"resume", "--last", "--json"})},
		{"fork approval bypass preserved", []string{"exec", "-sread-only", "fork", "session-id", "--dangerously-bypass-approvals-and-sandbox", "prompt"}, with([]string{"exec"}, []string{"fork", "session-id", "--dangerously-bypass-approvals-and-sandbox", "prompt"})},
		{"review option values", []string{"exec", "review", "--base=--sandbox", "--title", "title --sandbox read-only", "--skip-git-repo-check", "prompt"}, with([]string{"exec"}, []string{"review", "--base=--sandbox", "--title", "title --sandbox read-only", "prompt"})},
		{"resume single image value", []string{"exec", "resume", "--image", "image.png", "session-id", "prompt", "--skip-git-repo-check"}, with([]string{"exec"}, []string{"resume", "--image", "image.png", "session-id", "prompt"})},
		{"nested after exec prompt", []string{"exec", "initial prompt", "-sread-only", "resume", "--last"}, with([]string{"exec"}, []string{"initial prompt", "resume", "--last"})},
		{"approve for me override preserves bypass", []string{"--approve-for-me", "--dangerously-bypass-approvals-and-sandbox", "exec", "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust"}, with([]string{"--dangerously-bypass-approvals-and-sandbox", "exec"}, []string{"--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust"})},
		{"unrelated options retained", []string{"--ask-for-approval", "never", "--search", "exec", "--output-schema", "result.json", "--thread-source", "automation", "--color", "never", "--ephemeral", "--ignore-user-config", "--ignore-rules"}, with([]string{"--ask-for-approval", "never", "--search", "exec"}, []string{"--output-schema", "result.json", "--thread-source", "automation", "--color", "never", "--ephemeral", "--ignore-user-config", "--ignore-rules"})},
		{"image command-like value", []string{"--image", "exec", "e", "--model", "m", "exec", "prompt"}, with([]string{"--image", "exec", "e", "--model", "m", "exec"}, []string{"prompt"})},
		{"global image boundary retained", []string{"--image", "a.png", "b.png", "--sandbox", "read-only", "exec", "prompt"}, with([]string{"--image=a.png", "--image=b.png", "exec"}, []string{"prompt"})},
		{"global short image boundary retained", []string{"-i", "a.png", "exec", "-sread-only", "e", "prompt"}, with([]string{"--image=a.png", "--image=exec", "e"}, []string{"prompt"})},
		{"exec image boundary retained", []string{"exec", "--image", "a.png", "b.png", "--sandbox=read-only", "resume", "--last"}, with([]string{"exec"}, []string{"--image=a.png", "--image=b.png", "resume", "--last"})},
		{"image skip git boundary retained", []string{"exec", "-i", "a.png", "--skip-git-repo-check", "prompt"}, with([]string{"exec"}, []string{"--image=a.png", "prompt"})},
		{"image policy override boundary retained", []string{"exec", "--image", "a.png", "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox", "prompt"}, with([]string{"exec"}, []string{"--image=a.png", "--dangerously-bypass-approvals-and-sandbox", "prompt"})},
		{"unchanged variadic image boundary", []string{"exec", "--image", "a.png", "b.png", "--json", "prompt", "-sread-only"}, with([]string{"exec"}, []string{"--image", "a.png", "b.png", "--json", "prompt"})},
		{"attached image retains prompt", []string{"exec", "--image=--sandbox", "-iimage.png", "prompt", "--sandbox=read-only"}, with([]string{"exec"}, []string{"--image=--sandbox", "-iimage.png", "prompt"})},
		{"help preserves uninterpreted tail", []string{"exec", "--sandbox=read-only", "--help", "--future-option", "--sandbox=workspace-write"}, with([]string{"exec"}, []string{"--help", "--future-option", "--sandbox=workspace-write"})},
		{"help bundle", []string{"exec", "-hsread-only"}, with([]string{"exec"}, []string{"-hsread-only"})},
		{"version bundle", []string{"exec", "-Vsread-only"}, with([]string{"exec"}, []string{"-Vsread-only"})},
		{"help child", []string{"exec", "help", "resume"}, with([]string{"exec"}, []string{"help", "resume"})},
		{"empty prompt", []string{"exec", "", "--sandbox=read-only"}, with([]string{"exec"}, []string{""})},
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

func TestMacOSExecArgsRejectsAmbiguousOptions(t *testing.T) {
	tests := [][]string{
		nil,
		{"app-server"},
		{"--", "exec"},
		{"--image", "image.png", "exec"},
		{"--unknown", "exec"},
		{"exec", "--sandbox"},
		{"exec", "-s"},
		{"exec", "--model", "--sandbox", "read-only"},
		{"exec", "--future-option", "--sandbox", "read-only"},
		{"exec", "--future-option=--sandbox"},
		{"exec", "-x", "--sandbox=read-only"},
		{"exec", "--skip-git-repo-check=true"},
		{"exec", "--approve-for-me=true"},
		{"exec", "--dangerously-bypass-approvals-and-sandbox=true"},
		{"exec", "--help=true"},
		{"exec", "resume", "--sandbox", "read-only"},
		{"exec", "fork", "session-id", "-sread-only"},
		{"exec", "review", "--image", "image.png"},
		{"exec", "review", "--title", "--sandbox"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := macOSExecArgs(args); err == nil {
				t.Errorf("macOSExecArgs(%q) unexpectedly succeeded", args)
			}
		})
	}
}

func TestMacOSExecArgsErrorsDoNotExposePrompt(t *testing.T) {
	prompt := "private prompt text"
	_, err := macOSExecArgs([]string{"exec", prompt, "--sandbox"})
	if err == nil || strings.Contains(err.Error(), prompt) {
		t.Fatalf("expected an error without prompt contents, got %v", err)
	}
}
