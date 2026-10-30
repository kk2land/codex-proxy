package proxy

import "os"

func prepareLaunch(kind CommandKind, args []string, proxyDir string) (launchSpec, error) {
	if kind != CommandExec {
		return launchSpec{args: args}, nil
	}
	forcedArgs, err := macOSExecArgs(args)
	if err != nil {
		return launchSpec{}, err
	}
	return prepareSandboxLaunch(os.Getenv("CODEX_HOME"), proxyDir, forcedArgs, processSandboxed)
}

func prepareAppServerLaunch(request appServerStartup, proxyDir string) (launchSpec, error) {
	return prepareSandboxLaunch(environmentValue(request.Env, "CODEX_HOME"), proxyDir, appServerArgs(request.Args), processSandboxed)
}
