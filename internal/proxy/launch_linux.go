package proxy

func prepareLaunch(kind CommandKind, args []string) (launchSpec, error) {
	return launchSpec{args: args}, nil
}

func prepareAppServerLaunch(request appServerStartup) (launchSpec, error) {
	return launchSpec{args: appServerArgs(request.Args)}, nil
}
