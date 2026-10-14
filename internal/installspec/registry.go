package installspec

const (
	RegistryName   = "stackr-registry"
	RegistryVolume = "stackr-registry-data"
	// RegistryImage is the standard distribution registry.
	RegistryImage = "registry:3"
	// RegistryPort is the registry's port on the head's loopback. Nothing
	// else reaches it: the panel's registry front checks tokens and proxies.
	// A container on a bridge cannot dial the host's 127.0.0.1.
	RegistryPort = "5000"
)

// Registry is the head's image store: the stock registry image on the host's
// network, listening on 127.0.0.1 only, its data on a named volume. It does no
// auth of its own, so it must sit on no bridge: a published port would be
// open to every container there, and a tenant's build step is one.
func Registry() Container {
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
			// the storage path and the :5000 address.
			"REGISTRY_VALIDATION_MANIFESTS_INDEXES_PLATFORMS=none",
			// No blob descriptor cache: after a garbage collect the in-memory
			// one still answers for deleted blobs, so a re-import skips them
			// and a pull gets a broken stream. "none" is an unknown type,
			// which the registry takes as caching disabled.
			"REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR=none",
			// The image's own config says :5000, which on the host network
			// would be every interface.
			"REGISTRY_HTTP_ADDR=127.0.0.1:" + RegistryPort,
		},
		Labels:      map[string]string{LabelRole: "registry"},
		Volumes:     []string{RegistryVolume + ":/var/lib/registry"},
		HostNetwork: true,
		LogOpts:     logCap,
		Restart:     "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}
