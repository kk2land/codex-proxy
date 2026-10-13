package proxy

func prepareClaudeLaunch(kind claudeCommand, args []string) (launchSpec, error) {
	return launchSpec{args: args}, nil
}
