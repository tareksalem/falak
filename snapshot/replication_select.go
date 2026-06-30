package snapshot

import "github.com/libp2p/go-libp2p/core/peer"

// Candidate is a single replication target as seen by the holder. The
// node-side adapter assembles these from the phonebook (failure domain,
// Active status) and the metrics peer store (free disk, gravity-derived
// suitability). The snapshot package consumes the assembled values; it
// does not reach into phonebook/gravity itself, which keeps target
// selection a pure, table-testable function.
type Candidate struct {
	// NodeID is the libp2p peer ID string — the index/holder identity.
	NodeID string
	// PeerID is the resolved libp2p peer ID used to open the push stream.
	PeerID peer.ID
	// Datacenter / Region are the failure-domain identifiers. Datacenter
	// is preferred for spread; Region is the fallback when Datacenter is
	// empty; NodeID is the last-resort domain (every node distinct).
	Datacenter string
	Region     string
	// DiskMBFree is the candidate's free disk in MiB. Targets below the
	// configured headroom threshold are skipped — never push a large CRIU
	// archive onto a node that is nearly full.
	DiskMBFree int64
	// Gravity is a suitability score (higher is better) used only as the
	// power-of-two-choices tiebreak within a failure domain. It is a
	// relative preference, not an eligibility gate.
	Gravity float64
	// Active reports whether the phonebook considers the node healthy.
	Active bool
}

// failureDomain returns the candidate's failure-domain key: Datacenter,
// else Region, else NodeID. A non-empty domain lets selection spread
// replicas so the loss of one domain cannot take out every copy.
func (c Candidate) failureDomain() string {
	if c.Datacenter != "" {
		return c.Datacenter
	}
	if c.Region != "" {
		return c.Region
	}
	return c.NodeID
}

// selectTargets orders eligible replication targets best-first, optimizing
// for failure-domain spread first and per-domain suitability second.
//
// Selection order (plan part 3):
//  1. Filter: Active, not already a holder (exclude set keyed by NodeID),
//     and DiskMBFree >= diskHeadroomMB.
//  2. Failure-domain primary: emit at most one target per domain before
//     reusing any domain, and rank the holder's own domain last, so the
//     first copies land in domains distinct from the holder and from each
//     other.
//  3. Power-of-two-choices: within a domain, sample sampleSize candidates
//     and take the better by gravity — NOT the global gravity argmax,
//     which would herd every replica onto the few beefiest nodes.
//
// intn must return a value in [0, n). The Replicator passes a
// mutex-guarded rand source; tests pass a deterministic stub. The full
// ordered list is returned (not just K) so the caller can walk past
// failed targets until K succeed or candidates are exhausted.
func selectTargets(
	cands []Candidate,
	holderDC, holderRegion string,
	exclude map[string]bool,
	diskHeadroomMB int64,
	sampleSize int,
	intn func(int) int,
) []Candidate {
	holder := Candidate{Datacenter: holderDC, Region: holderRegion}
	holderDomain := holder.failureDomain()

	// Group eligible candidates by failure domain, preserving input order.
	groups := make(map[string][]Candidate)
	var domainOrder []string
	for _, c := range cands {
		if !c.Active {
			continue
		}
		if exclude[c.NodeID] {
			continue
		}
		if diskHeadroomMB > 0 && c.DiskMBFree < diskHeadroomMB {
			continue
		}
		dom := c.failureDomain()
		if _, ok := groups[dom]; !ok {
			domainOrder = append(domainOrder, dom)
		}
		groups[dom] = append(groups[dom], c)
	}
	if len(domainOrder) == 0 {
		return nil
	}

	// Rank domains: every domain except the holder's first (in first-seen
	// order), the holder's domain last so same-domain targets are a last
	// resort only when no other domain can satisfy K.
	ranked := make([]string, 0, len(domainOrder))
	var holderDomainPresent bool
	for _, d := range domainOrder {
		if d == holderDomain {
			holderDomainPresent = true
			continue
		}
		ranked = append(ranked, d)
	}
	if holderDomainPresent {
		ranked = append(ranked, holderDomain)
	}

	// Round-robin across domains: each round emits the best power-of-two
	// pick from every non-empty domain. The first round therefore yields
	// one target per distinct domain (maximum spread); later rounds reuse
	// domains only after each has contributed once.
	total := 0
	for _, c := range groups {
		total += len(c)
	}
	out := make([]Candidate, 0, total)
	for len(out) < total {
		progressed := false
		for _, dom := range ranked {
			members := groups[dom]
			if len(members) == 0 {
				continue
			}
			idx := powerOfTwoPick(members, sampleSize, intn)
			out = append(out, members[idx])
			// Remove the picked member (swap-with-last, order within a
			// domain is not significant beyond the power-of-two sample).
			members[idx] = members[len(members)-1]
			groups[dom] = members[:len(members)-1]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
}

// powerOfTwoPick samples up to sampleSize distinct members and returns the
// index of the highest-gravity one among the sample. With sampleSize < 2
// it degenerates to a uniform random pick; with sampleSize >= len it
// becomes the domain argmax. The default (2) gives the classic
// power-of-two-choices load smoothing.
func powerOfTwoPick(members []Candidate, sampleSize int, intn func(int) int) int {
	n := len(members)
	if n == 1 {
		return 0
	}
	s := sampleSize
	if s < 1 {
		s = 1
	}
	if s > n {
		s = n
	}
	seen := make(map[int]bool, s)
	bestIdx := -1
	// Bounded by n distinct draws; rejection sampling terminates because
	// we never request more distinct indices than exist.
	attempts := 0
	maxAttempts := n * 4
	for len(seen) < s && attempts < maxAttempts {
		attempts++
		idx := intn(n)
		if idx < 0 || idx >= n || seen[idx] {
			continue
		}
		seen[idx] = true
		if bestIdx == -1 || members[idx].Gravity > members[bestIdx].Gravity {
			bestIdx = idx
		}
	}
	if bestIdx == -1 {
		return 0
	}
	return bestIdx
}
