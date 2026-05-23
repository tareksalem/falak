// This file holds the Service-name answer path for the DNS server.
// Split out of server.go to keep that file focused on listener
// lifecycle and the capsule-name resolution path, and to keep the
// per-file LOC budget honest. The behaviour is invoked from
// server.handle when ServiceResolver admits a Service match.
package dns

import (
	"net"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// answerService writes the A response for a visibility-admitted
// Service match. Trivial Services return the replica A-records from
// the endpoints registry (the proxy-bypass path, decision #36);
// everything else returns the proxy listen IP so the caller talks to
// the local L4 proxy.
func (s *Server) answerService(w dns.ResponseWriter, resp *dns.Msg, q dns.Question, caller BridgeInfo, info ServiceDNSInfo, name string) {
	if info.Trivial {
		s.answerTrivialService(w, resp, q, caller, info, name)
		return
	}
	s.answerProxyService(w, resp, q, info, name)
}

// answerTrivialService writes the bypass-path response for a
// single-backend, weight-100, cluster, static Service.
//
// Trivial bypass: delegate to the capsule path using the resolver's
// reported capsule name + group. We do NOT re-apply DefaultPredicate
// here because the resolver already enforced Service visibility;
// re-applying group-private would block cluster-scoped trivials.
func (s *Server) answerTrivialService(w dns.ResponseWriter, resp *dns.Msg, q dns.Question, caller BridgeInfo, info ServiceDNSInfo, name string) {
	if s.registry == nil {
		resp.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(resp)
		return
	}
	eps := s.registry.Lookup(caller.ClusterPath, info.TrivialGroupID, info.TrivialName)
	if len(eps) == 0 {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}
	ips := make([]net.IP, 0, len(eps))
	for _, ep := range eps {
		if ip := net.ParseIP(ep.BridgeIP); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}
	rng := s.randSource()
	rng.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	ttl := uint32(s.ttlSeconds)
	for _, ip := range ips {
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
			A:   ip,
		})
	}
	s.writeOrLog(w, resp, name)
}

// answerProxyService writes the response that points the caller at
// the local L4 proxy IP. The proxy adds per-Service-port listeners
// on that IP; the client dials the Service port and the proxy
// SWRR-routes onto a replica.
func (s *Server) answerProxyService(w dns.ResponseWriter, resp *dns.Msg, q dns.Question, info ServiceDNSInfo, name string) {
	if !info.ProxyIP.IsValid() {
		resp.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(resp)
		return
	}
	ttl := uint32(s.ttlSeconds)
	resp.Answer = append(resp.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   info.ProxyIP.AsSlice(),
	})
	s.writeOrLog(w, resp, name)
}

// writeOrLog writes the response and logs a sampled warning on a write
// failure. Used by every success-path branch.
func (s *Server) writeOrLog(w dns.ResponseWriter, resp *dns.Msg, name string) {
	if err := w.WriteMsg(resp); err != nil {
		if s.errSampler.Allow("write_response") {
			s.logger.Warn("dns: write response failed",
				zap.String("name", name),
				zap.Int64("dropped", s.errSampler.Suppressed("write_response")),
				zap.Error(err))
		}
	}
}

