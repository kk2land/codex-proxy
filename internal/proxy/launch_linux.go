package proxy

func prepareLaunch(kind CommandKind, args []string) (launchSpec, error) {
	return launchSpec{args: args}, nil
}
