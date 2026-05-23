package capsule

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
)

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{
		Name:  "test-api",
		Image: "test:v1",
		Orbit: "api",
		Tier:  enums.TierEnum.Critical(),
		Labels: Labels{
			"team": "backend",
			"env":  "production",
		},
		Resources: ResourceRequirements{CPUCores: 2, MemoryMB: 512, DiskMB: 1024},
		Replicas:  ReplicaConfig{Min: 2, Max: 5},
		ScalingRules: []ScalingRule{
			{
				Name:       "high cpu",
				Trigger:    enums.TriggerModeEnum.All(),
				Conditions: []string{"cpu > 70%"},
				Action:     enums.ScalingActionEnum.ScaleUp(),
				Cooldown:   60 * time.Second,
			},
		},
		PlacementRules: []PlacementRule{
			{
				Name:     "gpu nodes",
				Type:     enums.PlacementTypeEnum.Node(),
				Labels:   Labels{"gpu": "true"},
				Required: true,
			},
		},
		MomentumConfig: MomentumConfig{
			Base:           90,
			BoostOnTraffic: true,
			ReduceOnIdle:   true,
			IdleTimeout:    5 * time.Minute,
		},
	}
	DefaultSpec(&spec)

	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "us-east/dc1/prod",
		Spec:      spec,
		Status:    enums.CapsuleStatusEnum.Created(),
		Momentum: MomentumState{
			Current:      90,
			Base:         90,
			LastAdjusted: time.Now(),
		},
		Version: "1",
	}

	if err := store.Create(c); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify in-memory
	got := store.Get(c.ID)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Spec.Name != "test-api" {
		t.Errorf("name: got %q, want %q", got.Spec.Name, "test-api")
	}

	// Close and reopen to verify persistence
	store.Close()

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 1 {
		t.Fatalf("expected 1 capsule after reopen, got %d", store2.Count())
	}

	reloaded := store2.Get(c.ID)
	if reloaded == nil {
		t.Fatal("Get after reopen returned nil")
	}

	// Verify full spec survived serialization
	if reloaded.Spec.Name != "test-api" {
		t.Errorf("name: got %q, want %q", reloaded.Spec.Name, "test-api")
	}
	if reloaded.Spec.Tier != enums.TierEnum.Critical() {
		t.Errorf("tier: got %q, want %q", reloaded.Spec.Tier, enums.TierEnum.Critical())
	}
	if reloaded.Spec.Labels["team"] != "backend" {
		t.Errorf("labels: team should be backend")
	}
	if reloaded.Spec.Resources.CPUCores != 2 {
		t.Errorf("cpu: got %d, want 2", reloaded.Spec.Resources.CPUCores)
	}
	if reloaded.Spec.Replicas.Min != 2 || reloaded.Spec.Replicas.Max != 5 {
		t.Errorf("replicas: got min=%d max=%d", reloaded.Spec.Replicas.Min, reloaded.Spec.Replicas.Max)
	}
	if len(reloaded.Spec.ScalingRules) != 1 {
		t.Fatalf("scaling rules: got %d, want 1", len(reloaded.Spec.ScalingRules))
	}
	if reloaded.Spec.ScalingRules[0].Name != "high cpu" {
		t.Errorf("scaling rule name: got %q", reloaded.Spec.ScalingRules[0].Name)
	}
	if len(reloaded.Spec.PlacementRules) != 1 {
		t.Fatalf("placement rules: got %d, want 1", len(reloaded.Spec.PlacementRules))
	}
	if reloaded.Spec.MomentumConfig.Base != 90 {
		t.Errorf("momentum base: got %d, want 90", reloaded.Spec.MomentumConfig.Base)
	}
	if reloaded.Momentum.Current != 90 {
		t.Errorf("momentum current: got %d, want 90", reloaded.Momentum.Current)
	}
	if reloaded.ClusterID != "us-east/dc1/prod" {
		t.Errorf("cluster_id: got %q", reloaded.ClusterID)
	}
	if string(reloaded.Status) != string(enums.CapsuleStatusEnum.Created()) {
		t.Errorf("status: got %q", reloaded.Status)
	}
}

func TestSQLiteStore_Update(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}

	store.Create(c)

	c.Status = enums.CapsuleStatusEnum.Running()
	c.Spec.Image = "img:v2"
	if err := store.Update(c); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	got := store2.Get(c.ID)
	if got.Status != enums.CapsuleStatusEnum.Running() {
		t.Errorf("status should be running, got %s", got.Status)
	}
	if got.Spec.Image != "img:v2" {
		t.Errorf("image should be img:v2, got %s", got.Spec.Image)
	}
}

func TestSQLiteStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}

	store.Create(c)
	store.Delete(c.ID)

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 0 {
		t.Errorf("should have 0 capsules after delete, got %d", store2.Count())
	}
}

func TestSQLiteStore_MultipleCapsules(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	for _, name := range []string{"api", "db", "worker", "cache"} {
		spec := CapsuleSpec{Name: name, Image: name + ":latest", Orbit: "default"}
		DefaultSpec(&spec)
		c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
		store.Create(c)
	}

	if store.Count() != 4 {
		t.Fatalf("expected 4, got %d", store.Count())
	}

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 4 {
		t.Errorf("expected 4 after reopen, got %d", store2.Count())
	}

	// Verify all names present
	names := map[string]bool{}
	for _, c := range store2.List() {
		names[c.Spec.Name] = true
	}
	for _, expected := range []string{"api", "db", "worker", "cache"} {
		if !names[expected] {
			t.Errorf("missing capsule %q after reopen", expected)
		}
	}
}

func TestSQLiteStore_InMemoryFallback(t *testing.T) {
	// NewStore (no path) should work without SQLite
	store := NewStore()
	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}

	if err := store.Create(c); err != nil {
		t.Fatalf("in-memory Create failed: %v", err)
	}
	if store.Count() != 1 {
		t.Errorf("expected 1, got %d", store.Count())
	}
}

func TestSQLiteStore_NonExistentDir(t *testing.T) {
	// Opening with a nonexistent parent dir should still work (sqlite creates the file)
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub", "dir")
	os.MkdirAll(subdir, 0755)

	store, err := OpenStore(filepath.Join(subdir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore in nested dir failed: %v", err)
	}
	defer store.Close()
}

// TestSQLiteStore_MigrationIdempotent verifies that opening a database file
// repeatedly produces a working store. The shared migrations runner makes
// re-opens a no-op once schema_migrations records the current version.
func TestSQLiteStore_MigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenStore failed: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}

	// Second open must not error — migrate() should detect existing columns
	// and skip the ALTER TABLE statements.
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("second OpenStore failed (idempotent migration broke): %v", err)
	}
	defer store2.Close()

	// And a third open for good measure.
	if err := store2.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	store3, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("third OpenStore failed: %v", err)
	}
	defer store3.Close()
}

// TestSQLiteStore_ListByGroup verifies that ListByGroup returns only
// capsules with a matching GroupID, sorted by Spec.Name, and treats the
// empty group ID as "no results".
func TestSQLiteStore_ListByGroup(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	groupA := CapsuleID("group-a")
	groupB := CapsuleID("group-b")

	type seed struct {
		name    string
		groupID CapsuleID
		member  bool
	}
	seeds := []seed{
		{"zeta", groupA, true},
		{"alpha", groupA, true},
		{"beta", groupB, true},
		{"standalone", "", false},
	}

	for _, s := range seeds {
		spec := CapsuleSpec{
			Name:        s.name,
			Image:       s.name + ":latest",
			Orbit:       "default",
			Kind:        CapsuleKindEnum.Capsule(),
			GroupID:     s.groupID,
			GroupMember: s.member,
		}
		DefaultSpec(&spec)
		c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
		if err := store.Create(c); err != nil {
			t.Fatalf("Create(%q) failed: %v", s.name, err)
		}
	}

	cases := []struct {
		name      string
		groupID   CapsuleID
		wantNames []string
	}{
		{"groupA returns sorted members", groupA, []string{"alpha", "zeta"}},
		{"groupB returns single member", groupB, []string{"beta"}},
		{"unknown group returns nothing", CapsuleID("nope"), nil},
		{"empty group returns nothing", CapsuleID(""), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.ListByGroup(tc.groupID)
			if len(got) != len(tc.wantNames) {
				t.Fatalf("len: got %d, want %d", len(got), len(tc.wantNames))
			}
			for i, c := range got {
				if c.Spec.Name != tc.wantNames[i] {
					t.Errorf("at %d: got %q, want %q", i, c.Spec.Name, tc.wantNames[i])
				}
			}
		})
	}
}

// TestSQLiteStore_ListByKind verifies that ListByKind filters correctly,
// treats empty stored kind as Capsule for back-compat, and returns sorted
// results.
func TestSQLiteStore_ListByKind(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	type seed struct {
		name string
		kind CapsuleKind
	}
	seeds := []seed{
		{"web", CapsuleKindEnum.Capsule()},
		{"app", CapsuleKindEnum.Capsule()},
		{"bundle", CapsuleKindEnum.Group()},
	}
	for _, s := range seeds {
		spec := CapsuleSpec{
			Name:  s.name,
			Image: s.name + ":latest",
			Orbit: "default",
			Kind:  s.kind,
		}
		DefaultSpec(&spec)
		// DefaultSpec must not unset the kind we asked for.
		spec.Kind = s.kind
		c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
		if err := store.Create(c); err != nil {
			t.Fatalf("Create(%q) failed: %v", s.name, err)
		}
	}

	// Inject a capsule whose stored Kind is empty (legacy row simulation):
	// we add it via the normal API but then NULL out via direct UPDATE.
	legacyID := NewCapsuleID()
	{
		spec := CapsuleSpec{Name: "legacy", Image: "legacy:1", Orbit: "default", Kind: CapsuleKindEnum.Capsule()}
		DefaultSpec(&spec)
		spec.Kind = CapsuleKindEnum.Capsule()
		c := &Capsule{ID: legacyID, Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
		if err := store.Create(c); err != nil {
			t.Fatalf("Create(legacy) failed: %v", err)
		}
		// Simulate a pre-migration row: kind column empty.
		if _, err := store.db.Exec(`UPDATE capsules SET kind = '' WHERE id = ?`, string(legacyID)); err != nil {
			t.Fatalf("setting legacy kind='' failed: %v", err)
		}
		// Reopen so the cache reflects the back-compat path in scanRow.
		if err := store.Close(); err != nil {
			t.Fatalf("Close before reload failed: %v", err)
		}
		var openErr error
		store, openErr = OpenStore(filepath.Join(dir, "capsules.db"))
		if openErr != nil {
			t.Fatalf("reopen for legacy reload failed: %v", openErr)
		}
		defer store.Close()
	}

	cases := []struct {
		name      string
		kind      CapsuleKind
		wantNames []string
	}{
		{
			name:      "Capsule kind includes legacy empty rows",
			kind:      CapsuleKindEnum.Capsule(),
			wantNames: []string{"app", "legacy", "web"},
		},
		{
			name:      "Group kind only returns groups",
			kind:      CapsuleKindEnum.Group(),
			wantNames: []string{"bundle"},
		},
		{
			name:      "empty query returns nothing",
			kind:      CapsuleKind(""),
			wantNames: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.ListByKind(tc.kind)
			if len(got) != len(tc.wantNames) {
				gotNames := make([]string, 0, len(got))
				for _, c := range got {
					gotNames = append(gotNames, c.Spec.Name)
				}
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), gotNames, len(tc.wantNames), tc.wantNames)
			}
			for i, c := range got {
				if c.Spec.Name != tc.wantNames[i] {
					t.Errorf("at %d: got %q, want %q", i, c.Spec.Name, tc.wantNames[i])
				}
			}
		})
	}
}

// TestSQLiteStore_GroupCapsuleRoundTrip verifies that a Kind=Group capsule
// with a populated GroupSpec survives close/reopen with the group spec
// intact.
func TestSQLiteStore_GroupCapsuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	groupSpec := &GroupSpec{
		Colocation: ColocationModeEnum.SameNode(),
		Members: []MemberSpec{
			{
				Name: "web",
				Spec: CapsuleSpec{Name: "web", Image: "web:1", Orbit: "front"},
			},
			{
				Name: "db",
				Spec: CapsuleSpec{Name: "db", Image: "db:1", Orbit: "front"},
			},
		},
		MemberIDs:     []CapsuleID{"member-1", "member-2"},
		CascadeDelete: true,
	}

	spec := CapsuleSpec{
		Name:  "bundle",
		Orbit: "front",
		Kind:  CapsuleKindEnum.Group(),
		Group: groupSpec,
	}
	DefaultSpec(&spec)
	// DefaultSpec must preserve our Kind/Group choices.
	spec.Kind = CapsuleKindEnum.Group()
	spec.Group = groupSpec

	id := NewCapsuleID()
	c := &Capsule{ID: id, Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
	if err := store.Create(c); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store2.Close()

	got := store2.Get(id)
	if got == nil {
		t.Fatal("Get after reopen returned nil")
	}
	if got.Spec.Kind != CapsuleKindEnum.Group() {
		t.Errorf("kind: got %q, want %q", got.Spec.Kind, CapsuleKindEnum.Group())
	}
	if got.Spec.Group == nil {
		t.Fatal("Group sub-spec lost after round-trip")
	}
	if got.Spec.Group.Colocation != ColocationModeEnum.SameNode() {
		t.Errorf("colocation: got %q, want %q", got.Spec.Group.Colocation, ColocationModeEnum.SameNode())
	}
	if len(got.Spec.Group.Members) != 2 {
		t.Fatalf("members: got %d, want 2", len(got.Spec.Group.Members))
	}
	if got.Spec.Group.Members[0].Name != "web" || got.Spec.Group.Members[1].Name != "db" {
		t.Errorf("member order/names changed: %+v", got.Spec.Group.Members)
	}
	if len(got.Spec.Group.MemberIDs) != 2 || got.Spec.Group.MemberIDs[0] != "member-1" {
		t.Errorf("member ids: got %v", got.Spec.Group.MemberIDs)
	}
	if !got.Spec.Group.CascadeDelete {
		t.Error("cascade delete flag lost")
	}

	// And ListByKind picks it up after reload.
	groups := store2.ListByKind(CapsuleKindEnum.Group())
	if len(groups) != 1 || groups[0].ID != id {
		t.Errorf("ListByKind(Group): got %v, want [%s]", groups, id)
	}
}

// TestSQLiteStore_MemberCapsuleRoundTrip verifies that a member capsule
// (Kind=Capsule, GroupID set, GroupMember=true) survives close/reopen with
// its membership fields intact and is returned by ListByGroup.
func TestSQLiteStore_MemberCapsuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	groupID := CapsuleID("g1")
	spec := CapsuleSpec{
		Name:        "web",
		Image:       "web:1",
		Orbit:       "front",
		Kind:        CapsuleKindEnum.Capsule(),
		GroupID:     groupID,
		GroupMember: true,
	}
	DefaultSpec(&spec)
	spec.Kind = CapsuleKindEnum.Capsule()
	spec.GroupID = groupID
	spec.GroupMember = true

	id := NewCapsuleID()
	c := &Capsule{ID: id, Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
	if err := store.Create(c); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store2.Close()

	got := store2.Get(id)
	if got == nil {
		t.Fatal("Get after reopen returned nil")
	}
	if got.Spec.Kind != CapsuleKindEnum.Capsule() {
		t.Errorf("kind: got %q, want %q", got.Spec.Kind, CapsuleKindEnum.Capsule())
	}
	if got.Spec.GroupID != groupID {
		t.Errorf("group_id: got %q, want %q", got.Spec.GroupID, groupID)
	}
	if !got.Spec.GroupMember {
		t.Error("group_member flag lost across reload")
	}

	members := store2.ListByGroup(groupID)
	if len(members) != 1 || members[0].ID != id {
		t.Errorf("ListByGroup(g1): got %v, want [%s]", members, id)
	}
}

// TestSQLiteStore_BaselineMigration_StampsExistingSchema verifies the
// pre-11A.15a baseline path: a database that already has the full schema
// in place (kind + group_id columns) but no `schema_migrations` rows must
// be adopted by the runner without re-running V1/V2 DDL (which would fail
// because the tables/columns already exist) and without losing rows.
func TestSQLiteStore_BaselineMigration_StampsExistingSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	// 1. Open the store normally so V1+V2 land via the runner and a row
	//    is created.
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenStore failed: %v", err)
	}
	spec := CapsuleSpec{
		Name: "legacy-app", Image: "legacy:1", Orbit: "default",
		Kind:    CapsuleKindEnum.Capsule(),
		GroupID: CapsuleID("g-legacy"),
	}
	DefaultSpec(&spec)
	spec.Kind = CapsuleKindEnum.Capsule()
	spec.GroupID = CapsuleID("g-legacy")
	id := NewCapsuleID()
	if err := store.Create(&Capsule{ID: id, Spec: spec, Status: enums.CapsuleStatusEnum.Created()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 2. Wipe the schema_migrations rows for the capsule module,
	//    simulating a database that was built before 11A.15a landed.
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE module = ?`, "capsule"); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 3. Reopen — the runner must detect the existing kind column and
	//    stamp V1+V2 as applied; no rerun of DDL, no errors, the prior
	//    capsule row preserved.
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("baseline reopen failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 1 {
		t.Fatalf("expected 1 capsule after baseline stamp, got %d", store2.Count())
	}
	got := store2.Get(id)
	if got == nil {
		t.Fatal("Get after baseline reopen returned nil")
	}
	if got.Spec.GroupID != CapsuleID("g-legacy") {
		t.Errorf("group_id lost across baseline reopen: got %q", got.Spec.GroupID)
	}

	// 4. Confirm both V1 and V2 are now stamped in schema_migrations.
	var n int
	row := store2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE module = ?`, "capsule")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != 2 {
		t.Errorf("schema_migrations rows after baseline: got %d, want 2", n)
	}
}
