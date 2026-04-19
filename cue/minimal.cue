// Minimal configuration for a single-node cluster.
// This is the simplest possible config — just a name, cluster, and PSK.

package cue

#Node & {
	name: "node1"
	port: 4001

	clusters: {
		"dev/local": #Cluster & {
			psk: "dev-local-psk-must-be-32-chars!!"
		}
	}
}
