package service

// Config is everything service.New needs to build the service tree.
// Filled in by step 1.
type Config struct{}

// Orchestrator is the single door into the service tree: the only thing
// main, the API and the web handlers see. One method per user-facing verb.
type Orchestrator struct{}

// New builds the orchestrator and everything below it (store, Docker
// client, leaves, flows).
func New(cfg Config) (*Orchestrator, error) {
	return &Orchestrator{}, nil
}
