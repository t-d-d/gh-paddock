// Package serve sets up each served repo's pools on GitHub and runs one
// message session per pool's scale set.
//
// GitHub is the queue (docs/constraints/github-queue.md): jobs wait in
// GitHub until a pool has room, and the listener reads its demand from the
// session's statistics alone.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/repos"
)

// Installation is the App's REST calls on one org.
type Installation interface {
	Repo(ctx context.Context, owner, name string) (github.Repo, error)
	EnsureRunnerGroup(ctx context.Context, org, name string, repoID int64) error
}

// App finds the App's installation on an org, and its id.
type App interface {
	Installation(ctx context.Context, org string) (Installation, int64, error)
}

// Session is one message session on a scale set.
type Session interface {
	listener.Client
	Close(ctx context.Context) error
}

// ScaleSets is the scale-set client of one org.
type ScaleSets interface {
	GetRunnerGroupByName(ctx context.Context, name string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// Pool is a pool the listener serves: its repo, its settings, its scale set
// and the scale-set client of its org.
type Pool struct {
	Repo       repos.Repo
	Pool       repos.Pool
	ScaleSetID int
	ScaleSets  ScaleSets
}

// Setup makes each listed repo's pools ready on GitHub: for each pool a
// runner group open to that repo alone, with public repos off, and a scale
// set in it. A repo it cannot serve is logged and skipped, and the rest are
// served. It returns the pools to serve.
func Setup(ctx context.Context, app App, newScaleSets func(org string, installationID int64) (ScaleSets, error), list repos.List, log *slog.Logger) []Pool {
	for _, err := range list.Skipped {
		log.Error("repo skipped: its entry in the list is not valid", "error", err.Error())
	}
	type org struct {
		inst Installation
		sets ScaleSets
		err  error
	}
	orgs := map[string]*org{}
	var pools []Pool
	for _, repo := range list.Repos {
		o, ok := orgs[repo.Owner]
		if !ok {
			o = &org{}
			var id int64
			o.inst, id, o.err = app.Installation(ctx, repo.Owner)
			if o.err == nil {
				o.sets, o.err = newScaleSets(repo.Owner, id)
			}
			orgs[repo.Owner] = o
		}
		if o.err != nil {
			log.Error("repo skipped", "repo", repo.FullName(), "error", o.err.Error())
			continue
		}
		served, err := setupRepo(ctx, o.inst, o.sets, repo)
		if err != nil {
			log.Error("repo skipped", "repo", repo.FullName(), "error", err.Error())
			continue
		}
		for _, p := range served {
			log.Info("pool ready", "repo", repo.FullName(), "pool", p.Pool.Name, "scaleSet", repo.ScaleSetName(p.Pool),
				"scaleSetId", p.ScaleSetID, "capacity", string(p.Pool.Capacity), "maxRunners", p.Pool.MaxRunners)
		}
		pools = append(pools, served...)
	}
	return pools
}

func setupRepo(ctx context.Context, inst Installation, sets ScaleSets, repo repos.Repo) ([]Pool, error) {
	r, err := inst.Repo(ctx, repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}
	if !r.Private {
		return nil, errors.New("the repo is public, and paddock serves private repos only")
	}
	var pools []Pool
	for _, pool := range repo.Pools {
		name := repo.ScaleSetName(pool)
		if err := inst.EnsureRunnerGroup(ctx, repo.Owner, name, r.ID); err != nil {
			return nil, fmt.Errorf("pool %s: runner group: %w", pool.Name, err)
		}
		group, err := sets.GetRunnerGroupByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("pool %s: runner group: %w", pool.Name, err)
		}
		want := &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: group.ID,
			Labels:        []scaleset.Label{{Name: name, Type: "System"}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		}
		set, err := sets.GetRunnerScaleSet(ctx, group.ID, name)
		switch {
		case err != nil:
		case set == nil:
			set, err = sets.CreateRunnerScaleSet(ctx, want)
		case !set.RunnerSetting.DisableUpdate:
			set, err = sets.UpdateRunnerScaleSet(ctx, set.ID, want)
		}
		if err != nil {
			return nil, fmt.Errorf("pool %s: scale set: %w", pool.Name, err)
		}
		pools = append(pools, Pool{Repo: repo, Pool: pool, ScaleSetID: set.ID, ScaleSets: sets})
	}
	return pools, nil
}

// beating passes each successful poll to beat: GitHub answered.
type beating struct {
	s    Session
	beat func()
}

func (b beating) DeleteMessage(ctx context.Context, id int) error { return b.s.DeleteMessage(ctx, id) }

func (b beating) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	return b.s.AcquireJobs(ctx, ids)
}

func (b beating) Session() scaleset.RunnerScaleSetSession { return b.s.Session() }

func (b beating) GetMessage(ctx context.Context, last, max int) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := b.s.GetMessage(ctx, last, max)
	if err == nil {
		b.beat()
	}
	return msg, err
}

// Backoff is the wait before a pool's next try, after its n-th failure in
// a row.
func Backoff(n int) time.Duration {
	return min(time.Duration(n)*5*time.Second, time.Minute)
}

// Run serves one pool until ctx is cancelled: it opens the pool's session,
// polls it, telling GitHub the pool's maximum each time, acquires the jobs
// each message offers, and hands the message to scaler before acking it. A
// refused session is retried with no limit; it beats, since GitHub
// answered, and the listener logs how long the refusal lasted. Any other
// failure closes the session and opens another after a wait, which grows
// with each failure in a row until a session opens. On cancel it closes the
// session.
func Run(ctx context.Context, pool Pool, open func(context.Context) (Session, error), scaler listener.Scaler, beat func(), log *slog.Logger, backoff func(int) time.Duration) {
	log = log.With("repo", pool.Repo.FullName(), "pool", pool.Pool.Name, "scaleSetId", pool.ScaleSetID)
	var refusedSince time.Time
	failures := 0
	wait := func() bool {
		failures++
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff(failures)):
			return true
		}
	}
	for ctx.Err() == nil {
		session, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, scaleset.ConflictError) {
				if refusedSince.IsZero() {
					refusedSince = time.Now()
				}
				beat()
				log.Warn("session refused", "refusedFor", time.Since(refusedSince).Round(time.Second).String(), "error", err.Error())
			} else {
				log.Error("could not open a session", "error", err.Error())
			}
			if !wait() {
				return
			}
			continue
		}
		failures = 0
		if !refusedSince.IsZero() {
			log.Info("session opened after a refusal", "refusedFor", time.Since(refusedSince).Round(time.Second).String())
			refusedSince = time.Time{}
		} else {
			log.Info("session opened")
		}
		err = runSession(ctx, pool, session, scaler, beat, log)
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if cerr := session.Close(closeCtx); cerr != nil {
			log.Warn("could not close the session", "error", cerr.Error())
		} else {
			log.Info("session closed")
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		log.Error("session ended", "error", err.Error())
		if !wait() {
			return
		}
	}
}

// acquiring acquires each job a message offers before scaling it: GitHub
// assigns a job to the scale set, and counts it in TotalAssignedJobs, only
// once the listener acquires it. Demand still comes from the statistics
// alone; acquiring counts nothing. Acquiring a job twice is harmless, so a
// redelivered message is safe.
type acquiring struct {
	client listener.Client
	scaler listener.Scaler
}

func (a acquiring) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg != nil && len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, job := range msg.JobAvailableMessages {
			ids = append(ids, job.RunnerRequestID)
		}
		if _, err := a.client.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquire offered jobs: %w", err)
		}
	}
	return a.scaler.Scale(ctx, msg)
}

func runSession(ctx context.Context, pool Pool, session Session, scaler listener.Scaler, beat func(), log *slog.Logger) error {
	client := beating{s: session, beat: beat}
	l, err := listener.New(client, listener.Config{
		ScaleSetID: pool.ScaleSetID,
		MaxRunners: pool.Pool.MaxRunners,
		Logger:     log.WithGroup("session"),
	})
	if err != nil {
		return err
	}
	beat()
	return l.Run(ctx, acquiring{client: client, scaler: scaler})
}
