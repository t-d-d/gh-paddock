package repos

import (
	"strings"
	"testing"
	"time"
)

func TestAnEmptyOrMissingListServesNothing(t *testing.T) {
	for _, raw := range []string{"", "  ", `{}`, `{"repos": []}`} {
		list, err := Parse(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if len(list.Repos) != 0 || len(list.Skipped) != 0 {
			t.Fatalf("%q serves %+v", raw, list)
		}
	}
}

func TestAPoolTakesGitHubsPrivateRunnerSizeTenRunnersAndTwoHourJobsByDefault(t *testing.T) {
	list, err := Parse(`{"repos": [{"repo": "acme/widgets", "pools": [{"name": "test", "capacity": "spot"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repos) != 1 {
		t.Fatalf("repos %+v, skipped %v", list.Repos, list.Skipped)
	}
	repo := list.Repos[0]
	if repo.Owner != "acme" || repo.Name != "widgets" {
		t.Fatalf("repo %+v", repo)
	}
	want := Pool{Name: "test", Capacity: Spot, Arch: X64, VCPU: 2, MemoryGB: 8, MaxRunners: 10, MaxJob: 2 * time.Hour}
	if len(repo.Pools) != 1 || repo.Pools[0] != want {
		t.Fatalf("pools %+v, want %+v", repo.Pools, want)
	}
}

func TestAPoolSetsItsOwnCapacitySizeMaximumAndJobTime(t *testing.T) {
	list, err := Parse(`{"repos": [{"repo": "acme/widgets", "pools": [
		{"name": "dev", "capacity": "on-demand", "arch": "arm64", "vcpu": 4, "memoryGb": 16, "maxRunners": 3, "maxJobMinutes": 45}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := Pool{Name: "dev", Capacity: OnDemand, Arch: ARM64, VCPU: 4, MemoryGB: 16, MaxRunners: 3, MaxJob: 45 * time.Minute}
	if got := list.Repos[0].Pools[0]; got != want {
		t.Fatalf("pool %+v, want %+v", got, want)
	}
}

func TestAPoolNamesX64OrArm64AsItsArchitecture(t *testing.T) {
	for raw, want := range map[string]Arch{`"x64"`: X64, `"arm64"`: ARM64} {
		list, err := Parse(`{"repos": [{"repo": "acme/widgets", "pools": [{"name": "test", "capacity": "spot", "arch": ` + raw + `}]}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Repos) != 1 || list.Repos[0].Pools[0].Arch != want {
			t.Fatalf("%s: repos %+v, skipped %v", raw, list.Repos, list.Skipped)
		}
	}
}

func TestAListThatIsNotJSONIsAnError(t *testing.T) {
	if _, err := Parse(`{"repos": [`); err == nil {
		t.Fatal("a broken list parsed")
	}
}

// One bad entry must not stop the rest being served.
func TestABadRepoIsSkippedWithItsReasonAndTheRestServed(t *testing.T) {
	cases := map[string]string{
		`{"repo": "no-slash", "pools": [{"name": "test", "capacity": "spot"}]}`:                                     "owner/name",
		`{"repo": "acme/x", "pools": []}`:                                                                           "no pools",
		`{"repo": "acme/x", "pools": [{"name": "test"}]}`:                                                           "capacity",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spotty"}]}`:                                     "capacity",
		`{"repo": "acme/x", "pools": [{"name": "Test!", "capacity": "spot"}]}`:                                      "pool name",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot"}, {"name": "test", "capacity": "spot"}]}`: "twice",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "vcpu": 3}]}`:                            "size",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "arch": "amd64"}]}`:                      "arch",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "arch": ""}]}`:                           "arch",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "vcpu": 1, "memoryGb": 16}]}`:            "size",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "maxRunners": -1}]}`:                     "maxRunners",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "maxRunners": 101}]}`:                    "maxRunners",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "maxJobMinutes": 0}]}`:                   "maxJobMinutes",
		`{"repo": "acme/x", "pools": [{"name": "test", "capacity": "spot", "maxJobMinutes": 7201}]}`:                "maxJobMinutes",
	}
	for entry, reason := range cases {
		list, err := Parse(`{"repos": [` + entry + `, {"repo": "acme/good", "pools": [{"name": "test", "capacity": "spot"}]}]}`)
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		if len(list.Repos) != 1 || list.Repos[0].Name != "good" {
			t.Errorf("%s: served %+v", entry, list.Repos)
		}
		if len(list.Skipped) != 1 || !strings.Contains(list.Skipped[0].Error(), reason) {
			t.Errorf("%s: skipped %v, want a reason with %q", entry, list.Skipped, reason)
		}
	}
}

func TestARepoListedTwiceIsServedOnce(t *testing.T) {
	list, err := Parse(`{"repos": [
		{"repo": "acme/widgets", "pools": [{"name": "test", "capacity": "spot"}]},
		{"repo": "Acme/Widgets", "pools": [{"name": "dev", "capacity": "spot"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repos) != 1 || len(list.Skipped) != 1 {
		t.Fatalf("repos %+v, skipped %v", list.Repos, list.Skipped)
	}
}

// Every pool belongs to one repo: its scale set and runner group carry the
// repo and the pool in their name, so no two pools share either.
func TestEachPoolHasItsOwnScaleSetName(t *testing.T) {
	t.Run("fitness: each pool has its own scale set and runner group, named for its repo (pool-per-repo.md)", func(t *testing.T) {
		repo := Repo{Owner: "acme", Name: "Widgets.js"}
		test := repo.ScaleSetName(Pool{Name: "test"})
		dev := repo.ScaleSetName(Pool{Name: "dev"})
		if test != "paddock-widgets-js-test" || dev != "paddock-widgets-js-dev" {
			t.Fatalf("names %q and %q", test, dev)
		}
		if got := repo.FullName(); got != "acme/Widgets.js" {
			t.Fatalf("full name %q", got)
		}
	})
}

// Two repos of one org whose names differ only in what the scale-set name
// folds away, or whose repo and pool names split one name two ways, would
// share a scale set and a runner group. The later entry is skipped.
func TestTwoPoolsThatWouldShareAScaleSetName(t *testing.T) {
	t.Run("fitness: no two pools share a scale set or runner group (pool-per-repo.md)", func(t *testing.T) {
		list, err := Parse(`{"repos": [
			{"repo": "acme/widgets", "pools": [{"name": "dev-test", "capacity": "spot"}]},
			{"repo": "acme/widgets-dev", "pools": [{"name": "test", "capacity": "spot"}]},
			{"repo": "acme/gadgets.js", "pools": [{"name": "test", "capacity": "spot"}]},
			{"repo": "Acme/gadgets-js", "pools": [{"name": "test", "capacity": "spot"}]},
			{"repo": "other/gadgets-js", "pools": [{"name": "test", "capacity": "spot"}]}]}`)
		if err != nil {
			t.Fatal(err)
		}
		var served []string
		for _, r := range list.Repos {
			served = append(served, r.FullName())
		}
		if len(list.Skipped) != 2 || strings.Join(served, " ") != "acme/widgets acme/gadgets.js other/gadgets-js" {
			t.Fatalf("served %v, skipped %v", served, list.Skipped)
		}
	})
}

// A typo in one entry must not read as the repo's removal: the listener
// holds a repo whose entry it skipped where it was, and drains nothing of it.
func TestASkippedEntryHoldsItsRepo(t *testing.T) {
	list, err := Parse(`{"repos": [
		{"repo": "acme/widgets", "pools": [{"name": "test", "capacity": "spto"}]},
		{"repo": "Acme/Gadgets", "pools": []},
		{"repo": "not a repo", "pools": [{"name": "test", "capacity": "spot"}]},
		{"repo": "acme/served", "pools": [{"name": "test", "capacity": "spot"}]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repos) != 1 || list.Repos[0].Name != "served" {
		t.Fatalf("serving %+v", list.Repos)
	}
	for _, name := range []string{"acme/widgets", "acme/gadgets", "ACME/Widgets"} {
		if !list.Holds(name) {
			t.Errorf("the list does not hold %s", name)
		}
	}
	if list.Holds("acme/served") || list.Holds("acme/other") {
		t.Error("the list holds a repo whose entry it did not skip")
	}
}

func TestAPoolsSettingsChangeUnderOneReaderAndOneWriter(t *testing.T) {
	c := NewCurrent(Pool{Name: "test", Capacity: Spot, MaxRunners: 10})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_ = c.Get()
		}
	}()
	c.Set(Pool{Name: "test", Capacity: OnDemand, MaxRunners: 3})
	<-done
	if got := c.Get(); got.Capacity != OnDemand || got.MaxRunners != 3 {
		t.Fatalf("settings %+v", got)
	}
}
