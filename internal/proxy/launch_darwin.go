package proxy

import "os"

func prepareLaunch(kind CommandKind, args []string) (launchSpec, error) {
	if kind != CommandExec {
		return launchSpec{args: args}, nil
	}
	profile, err := sandboxProfile(os.Getenv("CODEX_HOME"))
	if err != nil {
		return launchSpec{}, err
	}
	forcedArgs, err := macOSExecArgs(args)
	if err != nil {
		return launchSpec{}, err
	}
	return launchSpec{profile: profile, args: forcedArgs}, nil
}

func prepareAppServerLaunch(request appServerStartup) (launchSpec, error) {
	profile, err := sandboxProfile(environmentValue(request.Env, "CODEX_HOME"))
	if err != nil {
		return launchSpec{}, err
	}
	return launchSpec{profile: profile, args: appServerArgs(request.Args)}, nil
}
