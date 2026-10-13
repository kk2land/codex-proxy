package proxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestClaudeMacSandboxAndPermissionArguments(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/xxx/alice/.claude")
	args := append(append([]string{}, claudeStreamArgs...), "--permission-mode", "plan", "--permission-prompt-tool", "stdio")
	spec, err := prepareClaudeLaunch(claudeStream, args)
	if err != nil || !strings.Contains(spec.profile, `(subpath "/xxx/alice")`) || !reflect.DeepEqual(spec.args, args) {
		t.Fatalf("Claude launch changed permissions or sandbox: %+v %v", spec, err)
	}
	for _, invalid := range []string{"", "relative/.claude", "/xxx/alice/.codex", "/alice/.claude"} {
		t.Setenv("CLAUDE_CONFIG_DIR", invalid)
		if _, err := prepareClaudeLaunch(claudeStream, args); err == nil {
			t.Fatalf("accepted invalid Claude directory %q", invalid)
		}
	}
	pass, err := prepareClaudeLaunch(claudePassThrough, []string{"--version"})
	if err != nil || pass.profile != "" {
		t.Fatal("management command required a sandbox directory")
	}
}
