package testing

import falak "github.com/tareksalem/falak/cue"

capsule: falak.#Capsule & {
	name:  "api"
	image: "docker.io/library/nginx:alpine"
	orbit: "default"
	tier:  "standard"
	runtime: {
		network: {
			ports: [{name: falak.PortName.http, container: 80}]
		}

	}
	replicas: {min: 1, max: 10}
}
