package installspec

const (
	RegistryName   = "stackr-registry"
	RegistryVolume = "stackr-registry-data"
	// RegistryImage is the standard distribution registry.
	RegistryImage = "registry:3"
	// RegistryPort is the port in the names cluster registry images have on
	// a node (localhost:5000/...). Only a name: nothing listens there; the
	// controller loads every image into a node's Docker from the registry's
	// socket.
	RegistryPort = "5000"
	// registryRun is where the registry container sees RegistryDir.
	registryRun = "/run/stackr-registry"
)

// RegistryDir is the host directory holding the registry's socket: under the
// data dir, root only, so no tile reaches it. A host-network tile shares the
// host's loopback, so a TCP listener there would be open to it.
func RegistryDir(dataDir string) string { return dataDir + "/registry" }

// RegistrySocket is the registry's unix socket on the host.
func RegistrySocket(dataDir string) string { return RegistryDir(dataDir) + "/registry.sock" }

// Registry is the head's image store: the stock registry image with no
// network at all, serving a unix socket under the data dir. It does no auth
// of its own; only stackrd (root, the data dir mounted) talks to it.
func Registry(dataDir string) Container {
	c := Container{
		Name:  RegistryName,
		Image: RegistryImage,
		Env: []string{
			"REGISTRY_STORAGE_DELETE_ENABLED=true",
			// Store-time index check off: an imported index keeps its
			// upstream bytes with only the cluster's CPU types behind it, so
			// the other platforms' manifests are absent on purpose. The
			// importer guarantees the wanted ones. An env override rather
			// than a config file: neither create path (stackr-install, the
			// upgrade flow) places files, and the image's own config keeps
			// the storage path.
			"REGISTRY_VALIDATION_MANIFESTS_INDEXES_PLATFORMS=none",
			// No blob descriptor cache: after a garbage collect the in-memory
			// one still answers for deleted blobs, so a re-import skips them
			// and a pull gets a broken stream. "none" is an unknown type,
			// which the registry takes as caching disabled.
			"REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR=none",
			"REGISTRY_HTTP_NET=unix",
			"REGISTRY_HTTP_ADDR=" + registryRun + "/registry.sock",
		},
		Labels:  map[string]string{LabelRole: "registry"},
		Volumes: []string{RegistryVolume + ":/var/lib/registry", RegistryDir(dataDir) + ":" + registryRun},
		// No network: the image's debug server (:5001) and anything else it
		// opens stay inside the container.
		Network: "none",
		LogOpts: logCap,
		Restart: "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}
