package proxy

func prepareLaunch(kind CommandKind, args []string, proxyDir string) (launchSpec, error) {
	return launchSpec{args: args}, nil
}

func prepareAppServerLaunch(request appServerStartup, proxyDir string) (launchSpec, error) {
	return launchSpec{args: appServerArgs(request.Args)}, nil
}
