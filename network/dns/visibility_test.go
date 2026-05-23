package dns

import (
	"net/netip"
	"testing"
)

func TestDefaultPredicate(t *testing.T) {
	t.Parallel()
	caller := BridgeInfo{
		ClusterPath: "/falak/test",
		GroupID:     "billing",
		IP:          netip.MustParseAddr("10.42.0.1"),
	}
	cases := []struct {
		name          string
		callerGroupID string
		targetGroupID string
		want          bool
	}{
		{"same-group", "billing", "billing", true},
		{"different-group", "billing", "checkout", false},
		{"empty-caller", "", "billing", false},
		{"empty-target", "billing", "", false},
		{"both-empty", "", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := caller
			c.GroupID = tc.callerGroupID
			if got := DefaultPredicate(c, tc.targetGroupID); got != tc.want {
				t.Fatalf("DefaultPredicate(%q,%q) = %v want %v",
					tc.callerGroupID, tc.targetGroupID, got, tc.want)
			}
		})
	}
}

func TestAllowAllPredicate(t *testing.T) {
	t.Parallel()
	if !AllowAllPredicate(BridgeInfo{}, "") {
		t.Fatal("AllowAllPredicate must accept zero values")
	}
	if !AllowAllPredicate(BridgeInfo{GroupID: "a"}, "b") {
		t.Fatal("AllowAllPredicate must accept cross-group")
	}
}
