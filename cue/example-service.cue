// Operator-facing example showing how to declare Falak Services in a
// cluster config. Three patterns are illustrated:
//
//   - Static 90/10 weighted split with port_map shorthand.
//   - Canary auto-progression toward v2 at 10 % every 5 minutes.
//   - Blue-green flip with a 60-second drain window.
//
// Drop the relevant block into your cluster config's `services:` map.
// The CUE schema in falak.cue validates shape; the Go-side converter
// in config/service.go translates each entry into a service.ServiceSpec
// at node startup. See `.claude/plans/service-networking.md` for the
// full design.

package cue

// _example holds Service definitions used for documentation and
// fixture-style tests. The package compiles as-is so authors can
// iterate inside the IDE with full validation.
_example: {
	// Static 90/10 split — the simplest pattern. Both backends expose a
	// port called "http", so the port_map shorthand is omitted.
	payments_static: #Service & {
		name: "payments"
		ports: [{name: "http", port: 8080, protocol: "tcp"}]
		backends: [
			{capsule: "payments-v1", weight: 90},
			{capsule: "payments-v2", weight: 10},
		]
	}

	// Canary rollout from v1 to v2: auto-progression at 10 % every 5
	// minutes. abort_on snaps weights back if error rate exceeds 5 %.
	payments_canary: #Service & {
		name: "payments"
		ports: [{name: "http", port: 8080, protocol: "tcp"}]
		backends: [
			{capsule: "payments-v1", weight: 100},
			{capsule: "payments-v2", weight: 0},
		]
		strategy: {
			type: "canary"
			canary: {
				target:   "payments-v2"
				from:     "payments-v1"
				step:     10
				interval: "5m"
				abort_on: ["error_rate > 5%"]
			}
		}
	}

	// Blue-green flip — start with payments-v1 active. Edit `active:` to
	// payments-v2 and re-apply to cut over with a 60-second drain.
	payments_blue_green: #Service & {
		name: "payments"
		ports: [{name: "http", port: 8080, protocol: "tcp"}]
		backends: [
			{capsule: "payments-v1", weight: 100},
			{capsule: "payments-v2", weight: 0},
		]
		strategy: {
			type: "blue-green"
			blue_green: {
				active: "payments-v1"
				drain:  "60s"
			}
		}
	}
}
