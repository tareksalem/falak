// Multi-cluster configuration with mixed certificate modes.
// This node joins 3 clusters simultaneously, each with different PKI settings.

package cue

#Node & {
	name:       "edge-node-1"
	port:       4001
	region:     "eu-west"
	datacenter: "dc2"
	log_level:  "info"

	clusters: {
		// Cluster 1: External CA with voucher signing capability
		"prod/eu-west/dc2": #Cluster & {
			psk: "prod-eu-west-psk-32chars-minimum!"
			bootstrap: [
				"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWProd1",
			]
			certificates: #Certificates & {
				ca_cert: "/etc/falak/prod-eu/ca.crt"
				ca_key:  "/etc/falak/prod-eu/ca.key"
			}
		}

		// Cluster 2: External CA with pre-signed node cert (no CA key — can't sign for others)
		"prod/us-east/dc1": #Cluster & {
			psk: "prod-us-east-psk-32chars-minimum"
			bootstrap: [
				"/ip4/10.1.0.1/tcp/4001/p2p/12D3KooWProd2",
			]
			certificates: #Certificates & {
				ca_cert:   "/etc/falak/prod-us/ca.crt"
				node_cert: "/etc/falak/prod-us/edge-1.crt"
				node_key:  "/etc/falak/prod-us/edge-1.key"
			}
		}

		// Cluster 3: Auto mode (PSK-derived certs, no external CA)
		"staging/eu-west/dc2": #Cluster & {
			psk: "staging-eu-psk-at-least-32-chars!"
			bootstrap: [
				"/ip4/10.2.0.1/tcp/4001/p2p/12D3KooWStag1",
			]
		}
	}

	health: #Health & {
		protocol_period:    "3s"
		quarantine_timeout: "45s"
	}
}
