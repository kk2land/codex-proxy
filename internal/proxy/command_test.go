package proxy

import "testing"

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
		{"experimental json", []string{"exec", "--experimental-json", "prompt"}, CommandExec},
		{"exec help remains intercepted", []string{"exec", "--help"}, CommandExec},
		{"app server", []string{"app-server"}, CommandAppServer},
		{"app server tooling", []string{"app-server", "generate-json-schema"}, CommandAppServer},
		{"app server help remains intercepted", []string{"app-server", "--help"}, CommandAppServer},
		{"app server unknown option", []string{"app-server", "--future-option"}, CommandAppServer},
		{"app server after unknown option", []string{"--future-option", "app-server"}, CommandAppServer},
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
		{"unknown long option", []string{"--future-option", "exec"}, CommandExec},
		{"unknown long attached value", []string{"--future-option=exec", "app-server"}, CommandAppServer},
		{"unknown long before known value", []string{"--future-option", "--model", "exec", "app-server"}, CommandAppServer},
		{"unknown short option", []string{"-x", "exec"}, CommandExec},
		{"unknown short suffix", []string{"-xh", "exec"}, CommandExec},
		{"unknown short attached value", []string{"-xapp-server", "exec"}, CommandExec},
		{"exec-only option before command", []string{"--json", "exec"}, CommandExec},
		{"unknown option after prompt", []string{"prompt", "--future-option", "exec"}, CommandExec},
		{"invalid boolean value left to codex", []string{"--search=true", "exec"}, CommandExec},
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

func TestClassifyArgsLeavesInvalidArgumentsToCodex(t *testing.T) {
	tests := [][]string{
		{"--model"},
		{"--model", "--help"},
		{"--model", "--"},
		{"-c"},
		{"--image"},
		{"--image", "--help"},
		{"-i", "--oss", "exec"},
		{"--help=exec"},
		{"--version=exec"},
		{"prompt", "another prompt", "exec"},
		{"private initial prompt", "private additional prompt", "app-server"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			kind, err := ClassifyArgs(args)
			if err != nil {
				t.Fatalf("ClassifyArgs(%q) returned a proxy validation error: %v", args, err)
			}
			if kind != CommandPassThrough {
				t.Errorf("ClassifyArgs(%q) = %v, want %v", args, kind, CommandPassThrough)
			}
		})
	}
}

func TestClassifyArgsCommandIndex(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		want      CommandKind
		wantIndex int
	}{
		{"exec", []string{"exec", "--experimental-json"}, CommandExec, 0},
		{"option value before exec", []string{"--model", "exec", "exec"}, CommandExec, 2},
		{"option value before alias", []string{"-c", "e", "e"}, CommandExec, 2},
		{"prompt before exec", []string{"initial prompt", "exec"}, CommandExec, 1},
		{"unknown option and known value", []string{"--future=exec", "-m", "app-server", "e"}, CommandExec, 3},
		{"image values before exec", []string{"--image", "exec", "app-server", "--oss", "exec"}, CommandExec, 4},
		{"app server", []string{"--model", "exec", "app-server"}, CommandAppServer, 2},
		{"other command boundary", []string{"login", "exec"}, CommandPassThrough, -1},
		{"terminator", []string{"--", "exec"}, CommandPassThrough, -1},
		{"missing option value", []string{"--model", "--future", "exec"}, CommandPassThrough, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, index := classifyArgs(tt.args)
			if kind != tt.want || index != tt.wantIndex {
				t.Errorf("classifyArgs(%q) = (%v, %d), want (%v, %d)", tt.args, kind, index, tt.want, tt.wantIndex)
			}
		})
	}
}
