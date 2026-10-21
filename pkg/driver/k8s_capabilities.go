package driver

// k8sRuntimeCapabilityFacts declares what the Kubernetes driver actually
// writes into a sandbox Pod today.
//
// Every "not_configured" entry below is verified by the cross-assertion test
// against the Pod object createPod returns.
func k8sRuntimeCapabilityFacts() RuntimeCapabilityFacts {
	dimensions := []RuntimeCapabilityDimensionFacts{
		{
			Dimension:       dimensionResourceLimits,
			Mechanism:       reasonNotConfigured,
			Observed:        "the Pod container declares no ResourceRequirements requests or limits",
			DefaultBehavior: "an undeclared resource limit is not applied to the Pod",
		},
	}
	security := securityContextFacts(uniformSecurityContextReasons(reasonNotConfigured), securityContextObserved{
		CapabilityDrop: "the sandbox container's SecurityContext drops NET_RAW from its capability set",
		ReadOnlyRootfs: "the container SecurityContext is unset, so its root filesystem stays writable",
		NonRootUser:    "no PodSecurityContext runAsNonRoot or runAsUser is set, and ServiceAccountName is empty",
		UserNamespaces: "no PodSecurityContext is set, so no user-namespace remapping is requested",
	})
	for i := range security {
		if security[i].Dimension != dimensionCapabilityDrop {
			continue
		}
		security[i].Enforced = true
		security[i].Mechanism = mechanismK8sSecurityContext
		security[i].DefaultBehavior = "without the drop the sandbox could open raw sockets and ARP-spoof frames on a shared network"
	}
	dimensions = append(dimensions, security...)
	dimensions = append(dimensions,
		egressPolicyFacts(RuntimeDriverK8s),
		credentialPlaceholderFacts("the Pod path has no credential substitution of its own: every other declared environment value is written verbatim"),
		stoppedRuntimeRetentionFacts(RuntimeDriverK8s, "", "stopping a Pod deletes it and resume creates a new Pod from the image"),
		RuntimeCapabilityDimensionFacts{
			Dimension:       dimensionCheckpointRestore,
			Mechanism:       reasonUnsupported,
			Observed:        "no checkpoint or restore path exists for Pods",
			DefaultBehavior: "a sandbox cannot be checkpointed; the Pod is recreated from the image on resume",
		},
		RuntimeCapabilityDimensionFacts{
			Dimension:       dimensionGPUAndDevices,
			Mechanism:       reasonNotConfigured,
			Observed:        "the Pod container declares no nvidia.com/gpu or other device resource limit",
			DefaultBehavior: "no host GPU or device node is exposed to the Pod",
		},
	)
	return RuntimeCapabilityFacts{Driver: RuntimeDriverK8s, Dimensions: dimensions}
}
