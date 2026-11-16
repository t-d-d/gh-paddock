package serve

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/t-d-d/gh-paddock/internal/repos"
)

// drainSettle is how long after this listener stops a pool's polling it
// waits before it takes an empty task list for the pool's own: ECS can
// list a task a little after RunTask returns.
const drainSettle = 2 * time.Minute

// scaleSetPrefix starts every scale set and runner group paddock makes.
const scaleSetPrefix = "paddock-"

// Manager keeps the pools the listener serves in step with the repo list,
// each time the list is read (docs/constraints/served-repos.md).
//
// A pool new to the list is set up on GitHub and served. A pool whose
// settings changed takes them on its next poll and its next task. A pool
// gone from the list stops polling at once, so GitHub assigns it nothing
// more and its waiting tasks get no further config and exit by themselves
// (docs/constraints/remove-then-terminate.md). Its scale set is deleted
// once ECS shows none of its tasks left. A repo whose entry the list
// skipped is held as it was.
//
// It keeps nothing a restart would lose: a removed pool's scale set is
// found again by its runner group, in each org it knows, and its tasks are
// counted in ECS (docs/constraints/github-queue.md).
type Manager struct {
	App          App
	NewScaleSets func(org string, installationID int64) (ScaleSets, error)
	// Serve serves one pool until ctx ends. The manager runs it in a
	// goroutine of its own.
	Serve func(ctx context.Context, p Pool)
	// LiveTasks counts a scale set's tasks that have not been asked to
	// stop.
	LiveTasks func(ctx context.Context, scaleSetID int) (int, error)
	Log       *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	orgs    map[string]*org
	served  map[string]*servedPool
	stopped map[string]time.Time
	serving atomic.Int32
	wg      sync.WaitGroup
}

type org struct {
	name string
	inst Installation
	sets ScaleSets
}

type servedPool struct {
	pool   Pool
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// poolKey names a pool across orgs: its org and its scale set.
func poolKey(owner, scaleSet string) string { return strings.ToLower(owner) + "/" + scaleSet }

// Serving is how many pools the listener serves.
func (m *Manager) Serving() int { return int(m.serving.Load()) }

// Wait waits for every pool's Serve to return, once ctx of each Apply ends.
func (m *Manager) Wait() { m.wg.Wait() }

// Apply brings the served pools in step with list. Pools it serves run
// until ctx ends. Call it from one goroutine only, and only with a list
// that was read: a failed read changes nothing.
func (m *Manager) Apply(ctx context.Context, list repos.List) {
	if m.served == nil {
		m.orgs, m.served, m.stopped = map[string]*org{}, map[string]*servedPool{}, map[string]time.Time{}
	}
	want := map[string]bool{}
	for _, repo := range list.Repos {
		for _, p := range repo.Pools {
			want[poolKey(repo.Owner, repo.ScaleSetName(p))] = true
		}
	}

	removed := map[string]*servedPool{}
	for key, sp := range m.served {
		if want[key] || list.Holds(sp.pool.Repo.FullName()) {
			continue
		}
		sp.cancel()
		removed[key] = sp
	}
	for key, sp := range removed {
		<-sp.done
		delete(m.served, key)
		m.stopped[key] = m.now()
		m.Log.Info("pool removed from the list; draining", "repo", sp.pool.Repo.FullName(), "pool", sp.pool.Pool.Name,
			"scaleSetId", sp.pool.ScaleSetID)
	}

	var added []repos.Repo
	byRepo := map[string]int{}
	for _, repo := range list.Repos {
		for _, p := range repo.Pools {
			sp, ok := m.served[poolKey(repo.Owner, repo.ScaleSetName(p))]
			if !ok {
				i, seen := byRepo[repo.FullName()]
				if !seen {
					i = len(added)
					byRepo[repo.FullName()] = i
					added = append(added, repos.Repo{Owner: repo.Owner, Name: repo.Name})
				}
				added[i].Pools = append(added[i].Pools, p)
				continue
			}
			if sp.pool.Pool != p {
				sp.pool.Settings.Set(p)
				sp.pool.Pool = p
				m.Log.Info("pool settings changed", "repo", repo.FullName(), "pool", p.Name, "capacity", string(p.Capacity),
					"arch", string(p.Arch), "vcpu", p.VCPU, "memoryGb", p.MemoryGB, "maxRunners", p.MaxRunners, "maxJob", p.MaxJob.String())
			}
		}
	}
	for _, repo := range added {
		m.add(ctx, repo)
	}
	m.serving.Store(int32(len(m.served)))
	m.sweep(ctx, want, list)
}

// org is the App's installation and scale-set client on owner, kept for
// later reads. One that fails a call is dropped and reached afresh.
func (m *Manager) org(ctx context.Context, owner string) (*org, error) {
	key := strings.ToLower(owner)
	if o, ok := m.orgs[key]; ok {
		return o, nil
	}
	inst, id, err := m.App.Installation(ctx, owner)
	if err != nil {
		return nil, err
	}
	sets, err := m.NewScaleSets(owner, id)
	if err != nil {
		return nil, err
	}
	o := &org{name: owner, inst: inst, sets: sets}
	m.orgs[key] = o
	return o, nil
}

// add sets up repo's pools, those not yet served, and serves them. A repo it
// cannot serve is logged and tried again on the next read.
func (m *Manager) add(ctx context.Context, repo repos.Repo) {
	o, err := m.org(ctx, repo.Owner)
	if err != nil {
		m.Log.Error("repo skipped", "repo", repo.FullName(), "error", err.Error())
		return
	}
	pools, err := setupRepo(ctx, o.inst, o.sets, repo)
	if err != nil {
		// The org's installation may be the stale one: reach it afresh.
		delete(m.orgs, strings.ToLower(repo.Owner))
		m.Log.Error("repo skipped", "repo", repo.FullName(), "error", err.Error())
		return
	}
	for _, p := range pools {
		p.Settings = repos.NewCurrent(p.Pool)
		m.Log.Info("pool ready", "repo", repo.FullName(), "pool", p.Pool.Name, "scaleSet", repo.ScaleSetName(p.Pool),
			"scaleSetId", p.ScaleSetID, "capacity", string(p.Pool.Capacity), "arch", string(p.Pool.Arch), "maxRunners", p.Pool.MaxRunners)
		pctx, cancel := context.WithCancel(ctx)
		sp := &servedPool{pool: p, cancel: cancel, done: make(chan struct{})}
		key := poolKey(repo.Owner, repo.ScaleSetName(p.Pool))
		m.served[key] = sp
		delete(m.stopped, key)
		m.wg.Go(func() {
			defer close(sp.done)
			m.Serve(pctx, p)
		})
	}
}

// sweep deletes the scale set of each pool gone from the list, in every org
// the listener knows, once none of its tasks is left. Only paddock's own
// scale sets are touched, and none of a repo the list holds.
func (m *Manager) sweep(ctx context.Context, want map[string]bool, list repos.List) {
	held := map[string][]string{} // org -> scale-set prefixes of its held repos
	for _, full := range list.Held() {
		owner, name, _ := strings.Cut(full, "/")
		held[owner] = append(held[owner], repos.Repo{Owner: owner, Name: name}.ScaleSetName(repos.Pool{}))
	}
	// Every org the list names, and every org reached before: one whose
	// last repo left the list may still hold a removed pool.
	owners := map[string]string{}
	for key, o := range m.orgs {
		owners[key] = o.name
	}
	for _, repo := range list.Repos {
		owners[strings.ToLower(repo.Owner)] = repo.Owner
	}
	for key, owner := range owners {
		o, err := m.org(ctx, owner)
		if err != nil {
			m.Log.Warn("could not reach an org to find removed pools", "org", owner, "error", err.Error())
			continue
		}
		groups, err := o.inst.RunnerGroups(ctx, o.name)
		if err != nil {
			delete(m.orgs, key)
			m.Log.Warn("could not list runner groups to find removed pools", "org", o.name, "error", err.Error())
			continue
		}
	groups:
		for _, name := range groups {
			k := poolKey(o.name, name)
			if !strings.HasPrefix(name, scaleSetPrefix) || want[k] || m.served[k] != nil {
				continue
			}
			for _, prefix := range held[key] {
				if strings.HasPrefix(name, prefix) {
					continue groups
				}
			}
			m.drain(ctx, o, name)
		}
	}
}

// drain deletes a removed pool's scale set if none of its tasks is left.
func (m *Manager) drain(ctx context.Context, o *org, name string) {
	key := poolKey(o.name, name)
	if at, ok := m.stopped[key]; ok && m.now().Sub(at) < drainSettle {
		return
	}
	log := m.Log.With("org", o.name, "scaleSet", name)
	group, err := o.sets.GetRunnerGroupByName(ctx, name)
	if err != nil {
		log.Warn("could not read a removed pool's runner group", "error", err.Error())
		return
	}
	set, err := o.sets.GetRunnerScaleSet(ctx, group.ID, name)
	if err != nil {
		log.Warn("could not read a removed pool's scale set", "error", err.Error())
		return
	}
	if set == nil {
		delete(m.stopped, key)
		return
	}
	log = log.With("scaleSetId", set.ID)
	n, err := m.LiveTasks(ctx, set.ID)
	if err != nil {
		log.Warn("could not count a removed pool's tasks", "error", err.Error())
		return
	}
	if n > 0 {
		log.Info("pool draining", "tasks", n)
		return
	}
	if err := o.sets.DeleteRunnerScaleSet(ctx, set.ID); err != nil {
		log.Warn("could not delete a removed pool's scale set", "error", err.Error())
		return
	}
	delete(m.stopped, key)
	log.Info("scale set deleted")
}
