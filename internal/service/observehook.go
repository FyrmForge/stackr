package service

import "context"

// observeDiskRefusal is the observe move's hook for Forget and Adopt: a
// refusal while observe's data is on the node's disk. This stub answers nil;
// the observe branch's diskmoveobserve.go defines the real one and this file
// goes away at the merge.
func (o *Orchestrator) observeDiskRefusal(ctx context.Context, nodeID string) error { return nil }
