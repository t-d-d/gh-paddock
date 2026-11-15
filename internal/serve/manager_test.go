package serve

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/repos"
)

// servedPools records the pools a manager serves: each runs until its
// context ends.
type servedPools struct {
	mu      sync.Mutex
	running map[string]Pool // by scale-set name
	starts  []string
	ended   []string
	cancel  context.CancelFunc
	ctx     context.Context
	m       *Manager
}

func (s *servedPools) serve(ctx context.Context, p Pool) {
	name := p.Repo.ScaleSetName(p.Pool)
	s.mu.Lock()
	s.running[name] = p
	s.starts = append(s.starts, name)
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	delete(s.running, name)
	s.ended = append(s.ended, name)
	s.mu.Unlock()
}

func (s *servedPools) waitFor(t *testing.T, n int) map[string]Pool {
	t.Helper()
	var out map[string]Pool
	waitFor(t, "pools served", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		out = map[string]Pool{}
		for k, v := range s.running {
			out[k] = v
		}
		return len(out) == n
	})
	return out
}

func (s *servedPools) startsOf() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.starts)
}

func (s *servedPools) endedOf() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ended)
}

func (s *servedPools) stop() {
	s.cancel()
	s.m.Wait()
}

// liveTasks is ECS's count of each scale set's live tasks.
type liveTasks struct {
	mu  sync.Mutex
	n   map[int]int
	err error
}

func (l *liveTasks) count(_ context.Context, id int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n[id], l.err
}

func (l *liveTasks) set(id, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n[id] = n
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newManager(app App, sets *fakeScaleSets, logs *syncBuffer) (*Manager, *servedPools) {
	m, served, _, _ := newManagerWith(app, sets, logs)
	return m, served
}

func newManagerWith(app App, sets *fakeScaleSets, logs *syncBuffer) (*Manager, *servedPools, *liveTasks, *clock) {
	ctx, cancel := context.WithCancel(context.Background())
	served := &servedPools{running: map[string]Pool{}, ctx: ctx, cancel: cancel}
	live := &liveTasks{n: map[int]int{}}
	clk := &clock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	m := &Manager{
		App:          app,
		NewScaleSets: func(string, int64) (ScaleSets, error) { return sets, nil },
		Serve:        served.serve,
		LiveTasks:    live.count,
		Log:          slog.New(slog.NewJSONHandler(logs, nil)),
		Now:          clk.Now,
	}
	served.m = m
	return m, served, live, clk
}

func widgetsList(pools ...repos.Pool) repos.List {
	return repos.List{Repos: []repos.Repo{{Owner: "acme", Name: "widgets", Pools: pools}}}
}

var (
	testPool = repos.Pool{Name: "test", Capacity: repos.Spot, VCPU: 2, MemoryGB: 8, MaxRunners: 10, MaxJob: 2 * time.Hour}
	devPool  = repos.Pool{Name: "dev", Capacity: repos.OnDemand, VCPU: 2, MemoryGB: 8, MaxRunners: 2, MaxJob: 2 * time.Hour}
)

func acme() (*fakeInstallation, *fakeScaleSets, *fakeApp) {
	inst := &fakeInstallation{repos: map[string]github.Repo{
		"acme/widgets": {ID: 7, Private: true},
		"acme/gadgets": {ID: 8, Private: true},
	}, groups: map[string]int64{}}
	sets := &fakeScaleSets{inst: inst, sets: map[string]*scaleset.RunnerScaleSet{}}
	return inst, sets, &fakeApp{orgs: map[string]*fakeInstallation{"acme": inst}}
}

func TestARepoOrPoolAddedToTheListIsServedWithNoRestart(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served := newManager(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool))
	served.waitFor(t, 1)
	list := widgetsList(testPool, devPool)
	list.Repos = append(list.Repos, repos.Repo{Owner: "acme", Name: "gadgets", Pools: []repos.Pool{testPool}})
	m.Apply(ctx, list)
	pools := served.waitFor(t, 3)
	for _, name := range []string{"paddock-widgets-test", "paddock-widgets-dev", "paddock-gadgets-test"} {
		if _, ok := pools[name]; !ok || sets.sets[name] == nil {
			t.Errorf("%s is not served", name)
		}
	}
	// The pool already served is not set up or started again.
	if got := served.startsOf(); len(got) != 3 || sets.nextID != 3 {
		t.Fatalf("started %v, made %d scale sets", got, sets.nextID)
	}
	if m.Serving() != 3 {
		t.Fatalf("serving %d", m.Serving())
	}
}

func TestAChangedPoolSettingReachesTheServedPoolWithNoRestart(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served := newManager(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool))
	before := served.waitFor(t, 1)["paddock-widgets-test"]
	changed := repos.Pool{Name: "test", Capacity: repos.OnDemand, VCPU: 4, MemoryGB: 16, MaxRunners: 3, MaxJob: time.Hour}
	m.Apply(ctx, widgetsList(changed))
	if got := before.Settings.Get(); got != changed {
		t.Fatalf("the pool's settings are %+v, want %+v", got, changed)
	}
	if got := served.startsOf(); len(got) != 1 {
		t.Fatalf("the pool was restarted: %v", got)
	}
	if !strings.Contains(logs.String(), `"msg":"pool settings changed"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// A removed pool stops polling at once, so its waiting tasks get no further
// config and exit by themselves. Its scale set goes once ECS shows none of
// its tasks left; a task still running a job holds it.
func TestARemovedPoolsScaleSetIsDeletedOnceItsTasksHaveFinished(t *testing.T) {
	inst, sets, app := acme()
	var logs syncBuffer
	m, served, live, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	devID := sets.sets["paddock-widgets-dev"].ID
	live.set(devID, 1)

	m.Apply(ctx, widgetsList(testPool))
	if got := served.endedOf(); !slices.Equal(got, []string{"paddock-widgets-dev"}) {
		t.Fatalf("stopped serving %v", got)
	}
	if m.Serving() != 1 {
		t.Fatalf("serving %d", m.Serving())
	}
	clk.advance(3 * time.Minute)
	m.Apply(ctx, widgetsList(testPool))
	if sets.sets["paddock-widgets-dev"] == nil {
		t.Fatal("the scale set was deleted under a running task")
	}
	if !strings.Contains(logs.String(), `"msg":"pool draining"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}

	live.set(devID, 0)
	clk.advance(3 * time.Minute)
	m.Apply(ctx, widgetsList(testPool))
	if sets.sets["paddock-widgets-dev"] != nil || !slices.Equal(sets.deleted, []string{"paddock-widgets-dev"}) {
		t.Fatalf("deleted %v", sets.deleted)
	}
	if sets.sets["paddock-widgets-test"] == nil {
		t.Fatal("the pool still listed lost its scale set")
	}
	if !strings.Contains(logs.String(), `"msg":"scale set deleted"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
	// Its runner group goes with it; the listed pool's stays.
	if _, ok := inst.groups["paddock-widgets-dev"]; ok {
		t.Fatal("the removed pool's runner group is left")
	}
	if _, ok := inst.groups["paddock-widgets-test"]; !ok {
		t.Fatal("the listed pool lost its runner group")
	}
}

// A runner group left with no scale set, as when its delete failed after
// the scale set's, goes on a later read.
func TestARemovedPoolsRunnerGroupLeftBehindGoesOnALaterRead(t *testing.T) {
	inst, sets, app := acme()
	var logs syncBuffer
	inst.groups["paddock-gone-test"] = 8
	m, served := newManager(app, sets, &logs)
	t.Cleanup(served.stop)
	m.Apply(served.ctx, widgetsList(testPool))
	served.waitFor(t, 1)
	if _, ok := inst.groups["paddock-gone-test"]; ok {
		t.Fatal("the group with no scale set is left")
	}
}

// ECS can list a task a little after RunTask returns, so a pool that stopped
// polling this minute is not yet taken for empty.
func TestARemovedPoolWaitsOutTheTaskListsLagBeforeItsScaleSetGoes(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served, _, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	m.Apply(ctx, widgetsList(testPool))
	if len(sets.deleted) != 0 {
		t.Fatalf("deleted %v as it stopped polling", sets.deleted)
	}
	clk.advance(time.Minute)
	m.Apply(ctx, widgetsList(testPool))
	if len(sets.deleted) != 0 {
		t.Fatalf("deleted %v a minute after it stopped polling", sets.deleted)
	}
	clk.advance(2 * time.Minute)
	m.Apply(ctx, widgetsList(testPool))
	if !slices.Equal(sets.deleted, []string{"paddock-widgets-dev"}) {
		t.Fatalf("deleted %v", sets.deleted)
	}
}

// The listener keeps nothing a restart loses: a new one finds a removed
// pool's scale set by its runner group, and drains it the same way. Groups
// that are not paddock's are left alone.
func TestARestartedListenerFindsAndDrainsARemovedPoolsScaleSet(t *testing.T) {
	inst, sets, app := acme()
	var logs syncBuffer
	ctx := context.Background()
	inst.groups["paddock-gadgets-test"] = 8
	inst.groups["Default"] = 0
	gone, _ := sets.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "paddock-gadgets-test"})
	other, _ := sets.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "Default"})

	m, served, live, _ := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx = served.ctx
	live.set(gone.ID, 2)
	live.set(other.ID, 0)
	m.Apply(ctx, widgetsList(testPool))
	served.waitFor(t, 1)
	if len(sets.deleted) != 0 {
		t.Fatalf("deleted %v while its tasks run", sets.deleted)
	}
	live.set(gone.ID, 0)
	m.Apply(ctx, widgetsList(testPool))
	if !slices.Equal(sets.deleted, []string{"paddock-gadgets-test"}) {
		t.Fatalf("deleted %v", sets.deleted)
	}
}

// A typo in a served repo's entry skips the entry; the repo is held as it
// was, still served and never drained.
func TestASkippedEntryLeavesItsRepoServed(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served, _, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	typo, err := repos.Parse(`{"repos": [{"repo": "acme/widgets", "pools": [{"name": "test", "capacity": "spto"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		m.Apply(ctx, typo)
		clk.advance(5 * time.Minute)
	}
	if got := served.endedOf(); len(got) != 0 || len(sets.deleted) != 0 {
		t.Fatalf("stopped %v, deleted %v over a typo", got, sets.deleted)
	}
}

// An emptied list is read as the operator's: every pool stops polling, and
// each scale set goes once its tasks are gone.
func TestAnEmptiedListDrainsEveryPool(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served, _, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	m.Apply(ctx, repos.List{})
	clk.advance(3 * time.Minute)
	m.Apply(ctx, repos.List{})
	slices.Sort(sets.deleted)
	if !slices.Equal(sets.deleted, []string{"paddock-widgets-dev", "paddock-widgets-test"}) || m.Serving() != 0 {
		t.Fatalf("deleted %v, serving %d", sets.deleted, m.Serving())
	}
}

// A failed count of a pool's tasks deletes nothing.
func TestAScaleSetIsNotDeletedWhenItsTasksCannotBeCounted(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served, live, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	live.err = errors.New("ECS throttled")
	m.Apply(ctx, widgetsList(testPool))
	clk.advance(10 * time.Minute)
	m.Apply(ctx, widgetsList(testPool))
	if len(sets.deleted) != 0 {
		t.Fatalf("deleted %v without a count", sets.deleted)
	}
}

// Pools removed together stop together: each one's Serve is cancelled
// before the manager waits for any, so one slow to close its session does
// not hold the others' polling.
func TestPoolsRemovedTogetherStopTogether(t *testing.T) {
	_, sets, app := acme()
	var logs syncBuffer
	m, served := newManager(app, sets, &logs)
	t.Cleanup(served.stop)
	var mu sync.Mutex
	ctxs := map[string]context.Context{}
	var late []string
	m.Serve = func(ctx context.Context, p Pool) {
		name := p.Repo.ScaleSetName(p.Pool)
		mu.Lock()
		ctxs[name] = ctx
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		others := maps.Clone(ctxs)
		mu.Unlock()
		for other, octx := range others {
			select {
			case <-octx.Done():
			case <-time.After(time.Second):
				mu.Lock()
				late = append(late, other)
				mu.Unlock()
			}
		}
	}
	ctx := served.ctx
	m.Apply(ctx, widgetsList(testPool, devPool))
	waitFor(t, "pools served", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ctxs) == 2
	})
	m.Apply(ctx, repos.List{})
	mu.Lock()
	defer mu.Unlock()
	if len(late) != 0 {
		t.Fatalf("%v kept polling while another removed pool stopped", late)
	}
}

// An org whose setup fails is reached afresh on the next read, as after the
// App is reinstalled with a new installation id.
func TestAnOrgThatFailsIsReachedAfreshOnTheNextRead(t *testing.T) {
	inst, sets, app := acme()
	var logs syncBuffer
	m, served := newManager(app, sets, &logs)
	t.Cleanup(served.stop)
	var made int
	m.NewScaleSets = func(string, int64) (ScaleSets, error) { made++; return sets, nil }
	delete(inst.repos, "acme/widgets")
	m.Apply(served.ctx, widgetsList(testPool))
	inst.repos["acme/widgets"] = github.Repo{ID: 7, Private: true}
	m.Apply(served.ctx, widgetsList(testPool))
	served.waitFor(t, 1)
	if made != 2 {
		t.Fatalf("made the org's client %d times, want afresh after the failure", made)
	}
}

// An org whose runner groups could not be listed is still swept on the
// next read.
func TestAnOrgWhoseGroupsFailedIsSweptOnTheNextRead(t *testing.T) {
	inst, sets, app := acme()
	var logs syncBuffer
	m, served, _, clk := newManagerWith(app, sets, &logs)
	t.Cleanup(served.stop)
	m.Apply(served.ctx, widgetsList(testPool, devPool))
	served.waitFor(t, 2)
	inst.groupsErr = errors.New("GitHub answered 502")
	m.Apply(served.ctx, widgetsList(testPool))
	inst.groupsErr = nil
	clk.advance(3 * time.Minute)
	m.Apply(served.ctx, widgetsList(testPool))
	if !slices.Equal(sets.deleted, []string{"paddock-widgets-dev"}) {
		t.Fatalf("deleted %v", sets.deleted)
	}
}
