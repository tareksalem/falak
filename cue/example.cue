// Full example configuration showing all available options.
// Copy this file and modify for your environment.

package cue

#Node & {
	name:       "worker-1"
	port:       4001
	region:     "us-east"
	datacenter: "dc1"
	log_level:  "debug"
	data_dir:   "/var/lib/falak/worker-1"

	clusters: {
		// Production cluster with external CA
		"prod/dc1": #Cluster & {
			psk: "production-psk-must-be-32-chars-long"
			bootstrap: [
				"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWExamplePeerID1",
				"/ip4/10.0.0.2/tcp/4001/p2p/12D3KooWExamplePeerID2",
			]
			certificates: #Certificates & {
				ca_cert:   "/etc/falak/prod/ca.crt"
				ca_key:    "/etc/falak/prod/ca.key"
				node_cert: "/etc/falak/prod/worker-1.crt"
				node_key:  "/etc/falak/prod/worker-1.key"
			}
		}

		// Staging cluster with auto mode (PSK-derived certs)
		"staging/dc1": #Cluster & {
			psk: "staging-psk-at-least-32-characters!"
			bootstrap: [
				"/ip4/10.1.0.1/tcp/4001/p2p/12D3KooWExamplePeerID3",
			]
		}
	}

	health: #Health & {
		protocol_period:    "2s"
		ping_timeout:       "500ms"
		quarantine_timeout: "30s"
		score_increment:    0.5
		max_responders:     2
	}
}
