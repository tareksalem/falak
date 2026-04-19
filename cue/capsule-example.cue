// Example capsule configurations for Falak.
//
// Usage:
//   cue eval capsule-example.cue

package cue

// A simple web API capsule with autoscaling.
webAPI: #Capsule & {
	name:  "web-api"
	image: "registry.example.com/web-api:v1.2"
	orbit: "api"
	tier:  "standard"

	labels: {
		app:     "web-api"
		team:    "backend"
		version: "v1.2"
	}

	resources: {
		cpu:    2
		memory: "512MB"
		disk:   "1GB"
	}

	replicas: {
		min: 2
		max: 10
	}

	scaling: rules: [
		{
			name:    "high load"
			trigger: "all"
			conditions: ["cpu > 70%", "memory > 60%"]
			action:   "scaleUp"
			cooldown: "60s"
		},
		{
			name:    "traffic spike"
			trigger: "any"
			conditions: ["latency.p95 > 200ms", "rps > 1000"]
			action:   "scaleUp"
			cooldown: "30s"
		},
		{
			name:    "idle"
			trigger: "all"
			conditions: ["cpu < 15%", "rps < 10"]
			action:   "scaleDown"
			cooldown: "120s"
		},
	]

	placement: [
		{
			name: "gpu nodes in us"
			type: "node"
			labels: {
				gpu:    "true"
				region: "us-east"
			}
		},
		{
			name: "same dc as db"
			type: "capsule"
			mode: "near"
			targets: ["capsule-db"]
			labels: {
				datacenter: "same"
				region:     "same"
			}
		},
	]

	runtime: {
		type: "containerd"
		env: {
			DATABASE_URL: "postgres://db:5432/app"
			LOG_LEVEL:    "info"
		}
		ports: ["8080:8080"]
		health: {
			endpoint: "/health"
			interval: "10s"
			timeout:  "5s"
		}
	}
}

// A critical database capsule with fixed replicas.
database: #Capsule & {
	name:  "capsule-db"
	image: "registry.example.com/postgres:15"
	orbit: "data"
	tier:  "critical"

	labels: {
		app:  "database"
		tier: "data"
	}

	resources: {
		cpu:    4
		memory: "4GB"
		disk:   "100GB"
	}

	replicas: exact: 3

	placement: [
		{
			name: "spread across datacenters"
			type: "datacenter"
			labels: tier: "production"
		},
		{
			name: "away from other db instances"
			type: "capsule"
			mode: "away"
			targets: ["capsule-db"]
			labels: node: "same"
		},
	]
}

// A background batch job that can scale to zero.
batchJob: #Capsule & {
	name:  "data-processor"
	image: "registry.example.com/processor:latest"
	orbit: "workers"
	tier:  "background"

	replicas: {
		min: 0
		max: 5
	}

	scaling: rules: [
		{
			name:       "queue backlog"
			trigger:    "any"
			conditions: ["queue_depth > 100"]
			action:     "scaleUp"
			cooldown:   "30s"
		},
		{
			name:       "empty queue"
			trigger:    "all"
			conditions: ["queue_depth == 0", "idle > 5m"]
			action:     "scaleToZero"
		},
	]

	advanced: momentum: {
		base:             25
		boost_on_traffic: true
		reduce_on_idle:   true
		idle_timeout:     "10m"
	}
}
