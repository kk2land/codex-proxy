package proxy

import "os/exec"

type launchSpec struct {
	profile string
	args    []string
}

func (spec launchSpec) command(realBin string) *exec.Cmd {
	if spec.profile == "" {
		return exec.Command(realBin, spec.args...)
	}
	args := []string{"-p", spec.profile, realBin}
	args = append(args, spec.args...)
	return exec.Command("/usr/bin/sandbox-exec", args...)
}
