package installspec

const (
	RegistryName   = "stackr-registry"
	RegistryVolume = "stackr-registry-data"
	// RegistryImage is the standard distribution registry.
	RegistryImage = "registry:3"
	// RegistryPort is the registry's port on the head's loopback. Nothing
	// else reaches it: the panel's registry front checks tokens and proxies.
	RegistryPort = "5000"
)

// Registry is the head's image store: the stock registry image, bound to
// 127.0.0.1 only, its data on a named volume. It does no auth of its own.
func Registry() Container {
	c := Container{
		Name:    RegistryName,
		Image:   RegistryImage,
		Env:     []string{"REGISTRY_STORAGE_DELETE_ENABLED=true"},
		Labels:  map[string]string{LabelRole: "registry"},
		Volumes: []string{RegistryVolume + ":/var/lib/registry"},
		Ports:   []string{"127.0.0.1:" + RegistryPort + ":5000"},
		Restart: "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}
