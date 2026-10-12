package service

// HoldBuild is builds.hold for the external tests.
func (o *Orchestrator) HoldBuild(nodeID, repo string) (release func()) {
	return o.builds.hold(nodeID, repo)
}
