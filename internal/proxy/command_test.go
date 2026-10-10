package proxy

import (
	"strings"
	"testing"
)

func TestClassifyArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want CommandKind
	}{
		{"no arguments", nil, CommandPassThrough},
		{"exec", []string{"exec"}, CommandExec},
		{"alias", []string{"e", "--json", "prompt"}, CommandExec},
		{"exec resume", []string{"exec", "resume", "--last"}, CommandExec},
		{"exec review", []string{"exec", "review", "--unknown"}, CommandExec},
		{"exec help remains intercepted", []string{"exec", "--help"}, CommandExec},
		{"app server", []string{"app-server"}, CommandAppServer},
		{"app server tooling", []string{"app-server", "generate-json-schema"}, CommandAppServer},
		{"app server help remains intercepted", []string{"app-server", "--help"}, CommandAppServer},
		{"other command prompt", []string{"review", "exec", "app-server"}, CommandPassThrough},
		{"other command options", []string{"login", "--unknown", "app-server"}, CommandPassThrough},
		{"apply alias", []string{"a", "exec"}, CommandPassThrough},
		{"help subcommand", []string{"help", "app-server"}, CommandPassThrough},
		{"long help", []string{"--help", "exec"}, CommandPassThrough},
		{"short help", []string{"-h", "app-server"}, CommandPassThrough},
		{"version", []string{"--version", "exec"}, CommandPassThrough},
		{"short version", []string{"-V", "exec"}, CommandPassThrough},
		{"help bundle", []string{"-hV", "exec"}, CommandPassThrough},
		{"version bundle", []string{"-Vh", "exec"}, CommandPassThrough},
		{"terminator", []string{"--", "exec"}, CommandPassThrough},
		{"terminator before app server", []string{"--", "app-server"}, CommandPassThrough},
		{"terminator after options", []string{"--search", "--", "exec"}, CommandPassThrough},
		{"prompt mentioning commands", []string{"run exec and app-server"}, CommandPassThrough},
		{"empty prompt", []string{""}, CommandPassThrough},
		{"stdin prompt", []string{"-"}, CommandPassThrough},
		{"subcommand after prompt", []string{"initial prompt", "exec", "--json"}, CommandExec},
		{"app server after prompt", []string{"initial prompt", "app-server"}, CommandAppServer},
		{"options after prompt", []string{"initial prompt", "--model", "exec", "app-server"}, CommandAppServer},
		{"single option value", []string{"--model", "exec"}, CommandPassThrough},
		{"option value before command", []string{"--model", "app-server", "exec"}, CommandExec},
		{"equal value", []string{"--model=exec", "app-server"}, CommandAppServer},
		{"attached short value", []string{"-mexec", "app-server"}, CommandAppServer},
		{"attached short equal value", []string{"-m=app-server", "exec"}, CommandExec},
		{"short value looks like bundle", []string{"-chV", "exec"}, CommandExec},
		{"repeated config", []string{"-c", "exec", "--config=app-server", "e"}, CommandExec},
		{"all single value options", []string{"--config", "exec", "--enable", "app-server", "--disable", "exec", "--remote", "exec", "--remote-auth-token-env", "app-server", "--model", "exec", "--local-provider", "app-server", "--profile", "exec", "--sandbox", "app-server", "--cd", "exec", "--add-dir", "app-server", "--ask-for-approval", "exec", "e"}, CommandExec},
		{"all short value options", []string{"-c", "exec", "-m", "app-server", "-p", "exec", "-s", "app-server", "-C", "exec", "-a", "app-server", "e"}, CommandExec},
		{"all boolean options", []string{"--strict-config", "--oss", "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust", "--worktree", "--search", "--no-alt-screen", "exec"}, CommandExec},
		{"image consumes command", []string{"--image", "image.png", "exec"}, CommandPassThrough},
		{"image consumes app server", []string{"-i", "first.png", "second.png", "app-server"}, CommandPassThrough},
		{"image command-like filename", []string{"--image", "exec"}, CommandPassThrough},
		{"image consumes dash prompt", []string{"--image", "-", "exec"}, CommandPassThrough},
		{"image ends at flag", []string{"--image", "image.png", "--model", "m", "exec"}, CommandExec},
		{"image ends at boolean flag", []string{"-i", "image.png", "--oss", "app-server"}, CommandAppServer},
		{"image equal does not consume command", []string{"--image=image.png", "exec"}, CommandExec},
		{"image attached does not consume command", []string{"-iimage.png", "exec"}, CommandExec},
		{"image attached equal does not consume command", []string{"-i=image.png", "app-server"}, CommandAppServer},
		{"image equal then prompt and command", []string{"--image=image.png", "prompt", "exec"}, CommandExec},
		{"image ends at terminator", []string{"--image", "image.png", "--", "exec"}, CommandPassThrough},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyArgs(tt.args)
			if err != nil {
				t.Fatalf("ClassifyArgs(%q): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("ClassifyArgs(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestClassifyArgsRejectsAmbiguousOrInvalidTopLevelOptions(t *testing.T) {
	tests := [][]string{
		{"--future-option", "exec"},
		{"--future-option=exec", "app-server"},
		{"-x", "exec"},
		{"-xh", "exec"},
		{"--json", "exec"}, // An exec-only option cannot precede its command.
		{"--model"},
		{"--model", "--help"},
		{"--model", "--"},
		{"-c"},
		{"--image"},
		{"--image", "--help"},
		{"-i", "--oss", "exec"},
		{"--help=exec"},
		{"--version=exec"},
		{"--search=true", "exec"},
		{"prompt", "another prompt", "exec"},
		{"prompt", "--future-option", "exec"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			if _, err := ClassifyArgs(args); err == nil {
				t.Errorf("ClassifyArgs(%q) unexpectedly succeeded", args)
			}
		})
	}
}

func TestClassifyArgsErrorDoesNotExposePrompt(t *testing.T) {
	first := "private initial prompt"
	second := "private additional prompt"
	_, err := ClassifyArgs([]string{first, second, "exec"})
	if err == nil {
		t.Fatal("multiple top-level prompt arguments unexpectedly accepted")
	}
	if strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
		t.Fatalf("classification error exposes a prompt: %v", err)
	}
}
