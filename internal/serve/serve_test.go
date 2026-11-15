package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/repos"
)

type fakeInstallation struct {
	repos  map[string]github.Repo
	groups map[string]int64 // runner group name -> repo it is open to
}

func (i *fakeInstallation) Repo(_ context.Context, owner, name string) (github.Repo, error) {
	r, ok := i.repos[owner+"/"+name]
	if !ok {
		return github.Repo{}, errors.New("GitHub answered 404")
	}
	return r, nil
}

func (i *fakeInstallation) EnsureRunnerGroup(_ context.Context, _, name string, repoID int64) error {
	i.groups[name] = repoID
	return nil
}

type fakeApp struct {
	orgs map[string]*fakeInstallation
}

func (a *fakeApp) Installation(_ context.Context, org string) (Installation, int64, error) {
	i, ok := a.orgs[org]
	if !ok {
		return nil, 0, fmt.Errorf("the App is not installed on %s", org)
	}
	return i, 1, nil
}

type fakeScaleSets struct {
	inst   *fakeInstallation
	sets   map[string]*scaleset.RunnerScaleSet
	nextID int
}

func (f *fakeScaleSets) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	if _, ok := f.inst.groups[name]; !ok {
		return nil, fmt.Errorf("no runner group found with name %q", name)
	}
	return &scaleset.RunnerGroup{ID: 40, Name: name}, nil
}

func (f *fakeScaleSets) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	return f.sets[name], nil
}

func (f *fakeScaleSets) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.nextID++
	c := *s
	c.ID = f.nextID
	f.sets[s.Name] = &c
	return &c, nil
}

func (f *fakeScaleSets) UpdateRunnerScaleSet(_ context.Context, id int, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	c := *s
	c.ID = id
	f.sets[s.Name] = &c
	return &c, nil
}

func (f *fakeScaleSets) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return nil, errors.New("not in this test")
}

func (f *fakeScaleSets) OpenSession(context.Context, int, string) (Session, error) {
	return nil, errors.New("not in this test")
}

func logger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

func TestEachPoolOfAServedRepoGetsAScaleSetAndARunnerGroupOpenToItAlone(t *testing.T) {
	inst := &fakeInstallation{repos: map[string]github.Repo{"acme/widgets": {ID: 7, Private: true}}, groups: map[string]int64{}}
	sets := &fakeScaleSets{inst: inst, sets: map[string]*scaleset.RunnerScaleSet{}}
	app := &fakeApp{orgs: map[string]*fakeInstallation{"acme": inst}}
	list := repos.List{Repos: []repos.Repo{{Owner: "acme", Name: "widgets", Pools: []repos.Pool{
		{Name: "test", Capacity: repos.Spot, VCPU: 2, MemoryGB: 8, MaxRunners: 10},
		{Name: "dev", Capacity: repos.OnDemand, VCPU: 2, MemoryGB: 8, MaxRunners: 2},
	}}}}
	var logs bytes.Buffer
	pools := Setup(context.Background(), app, func(string, int64) (ScaleSets, error) { return sets, nil }, list, logger(&logs))
	if len(pools) != 2 {
		t.Fatalf("serving %d pools; logs:\n%s", len(pools), logs.String())
	}
	for _, name := range []string{"paddock-widgets-test", "paddock-widgets-dev"} {
		if inst.groups[name] != 7 {
			t.Errorf("runner group %s is open to %d", name, inst.groups[name])
		}
		set := sets.sets[name]
		if set == nil {
			t.Fatalf("no scale set %s", name)
		}
		if set.RunnerGroupID != 40 || !set.RunnerSetting.DisableUpdate {
			t.Errorf("scale set %+v", set)
		}
	}
	if pools[1].Pool.Name != "dev" || pools[1].ScaleSetID == pools[0].ScaleSetID {
		t.Errorf("pools %+v", pools)
	}
	// Setup again finds both sets and creates none.
	before := sets.nextID
	if again := Setup(context.Background(), app, func(string, int64) (ScaleSets, error) { return sets, nil }, list, logger(&logs)); len(again) != 2 || sets.nextID != before {
		t.Fatalf("a second setup made %d more sets", sets.nextID-before)
	}
}

func TestARepoThatCannotBeServedIsLoggedAndSkipped(t *testing.T) {
	inst := &fakeInstallation{repos: map[string]github.Repo{
		"acme/widgets": {ID: 7, Private: true},
		"acme/open":    {ID: 8, Private: false},
	}, groups: map[string]int64{}}
	sets := &fakeScaleSets{inst: inst, sets: map[string]*scaleset.RunnerScaleSet{}}
	app := &fakeApp{orgs: map[string]*fakeInstallation{"acme": inst}}
	pool := []repos.Pool{{Name: "test", Capacity: repos.Spot, VCPU: 2, MemoryGB: 8, MaxRunners: 10}}
	list := repos.List{
		Repos: []repos.Repo{
			{Owner: "elsewhere", Name: "thing", Pools: pool},
			{Owner: "acme", Name: "gone", Pools: pool},
			{Owner: "acme", Name: "open", Pools: pool},
			{Owner: "acme", Name: "widgets", Pools: pool},
		},
		Skipped: []error{errors.New("entry 9: repo x/y has no pools")},
	}
	var logs bytes.Buffer
	pools := Setup(context.Background(), app, func(string, int64) (ScaleSets, error) { return sets, nil }, list, logger(&logs))
	if len(pools) != 1 || pools[0].Repo.Name != "widgets" {
		t.Fatalf("serving %+v", pools)
	}
	for _, want := range []string{"elsewhere/thing", "not installed", "acme/gone", "acme/open", "public", "entry 9"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the logs do not say %q:\n%s", want, logs.String())
		}
	}
	if _, ok := inst.groups["paddock-open-test"]; ok {
		t.Error("a public repo got a runner group")
	}
}

// fakeSession is one message session.
type fakeSession struct {
	id       uuid.UUID
	messages chan *scaleset.RunnerScaleSetMessage
	acked    []int
	max      []int
	acquired []int64
	closed   atomic.Bool
	mu       sync.Mutex
}

func (s *fakeSession) GetMessage(ctx context.Context, last, max int) (*scaleset.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	s.max = append(s.max, max)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m, ok := <-s.messages:
		if !ok {
			return nil, errors.New("the poll failed")
		}
		return m, nil
	}
}

func (s *fakeSession) DeleteMessage(_ context.Context, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = append(s.acked, id)
	return nil
}

func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquired = append(s.acquired, ids...)
	return ids, nil
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: s.id, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3}}
}

func (s *fakeSession) Close(context.Context) error {
	s.closed.Store(true)
	return nil
}

type recordingScaler struct {
	mu   sync.Mutex
	seen []int
}

func (r *recordingScaler) Scale(_ context.Context, m *scaleset.RunnerScaleSetMessage) error {
	if m == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, m.Statistics.TotalAssignedJobs)
	return nil
}

func (r *recordingScaler) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The session's opening statistics scale first, so jobs queued while no
// listener ran start; each message is acked after it is handled, each poll
// carries the pool's maximum and beats; and a cancel closes the session.
func TestAPoolPollsItsSessionAndClosesItOnCancel(t *testing.T) {
	session := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	scaler := &recordingScaler{}
	var beats atomic.Int32
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Pool{Repo: repos.Repo{Owner: "acme", Name: "widgets"}, Pool: repos.Pool{Name: "test", MaxRunners: 4}, ScaleSetID: 5},
			func(context.Context) (Session, error) { return session, nil }, scaler, func() { beats.Add(1) }, logger(&logs), func(int) time.Duration { return time.Millisecond })
		close(done)
	}()
	session.messages <- &scaleset.RunnerScaleSetMessage{MessageID: 11, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
	session.messages <- nil
	waitFor(t, "two handled polls", func() bool { return scaler.count() >= 2 && beats.Load() >= 2 })
	cancel()
	<-done
	if !session.closed.Load() {
		t.Fatal("the session was not closed on cancel")
	}
	if scaler.seen[0] != 3 || scaler.seen[1] != 1 {
		t.Fatalf("scaled to %v, want the session's 3 first", scaler.seen)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.acked) != 1 || session.acked[0] != 11 {
		t.Fatalf("acked %v", session.acked)
	}
	for _, m := range session.max {
		if m != 4 {
			t.Fatalf("a poll told GitHub a maximum of %d", m)
		}
	}
}

// GitHub allows one session per scale set, and refuses another while an
// old listener's session lives. The pool retries with no limit, stays
// healthy, since GitHub answered, and logs how long the refusal lasted.
func TestARefusedSessionIsRetriedAndItsLengthLogged(t *testing.T) {
	session := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	var tries atomic.Int32
	var beats atomic.Int32
	var logs syncBuffer
	scaler := &recordingScaler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Pool{Repo: repos.Repo{Owner: "acme", Name: "widgets"}, Pool: repos.Pool{Name: "test", MaxRunners: 4}, ScaleSetID: 5},
			func(context.Context) (Session, error) {
				if tries.Add(1) <= 5 {
					return nil, fmt.Errorf("create session: %w", scaleset.ConflictError)
				}
				return session, nil
			}, scaler, func() { beats.Add(1) }, slog.New(slog.NewJSONHandler(&logs, nil)), func(int) time.Duration { return time.Millisecond })
		close(done)
	}()
	waitFor(t, "the session after five refusals", func() bool { return scaler.count() >= 1 })
	cancel()
	<-done
	if beats.Load() < 5 {
		t.Fatalf("%d beats over five refusals", beats.Load())
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"session refused"`) || !strings.Contains(out, `"refusedFor"`) {
		t.Fatalf("logs:\n%s", out)
	}
}

// A poll that fails ends the session; the pool closes it and opens another.
func TestAFailedPollReopensTheSession(t *testing.T) {
	first := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	second := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	var opened atomic.Int32
	var logs syncBuffer
	scaler := &recordingScaler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Pool{Repo: repos.Repo{Owner: "acme", Name: "widgets"}, Pool: repos.Pool{Name: "test", MaxRunners: 4}, ScaleSetID: 5},
			func(context.Context) (Session, error) {
				if opened.Add(1) == 1 {
					return first, nil
				}
				return second, nil
			}, scaler, func() {}, slog.New(slog.NewJSONHandler(&logs, nil)), func(int) time.Duration { return time.Millisecond })
		close(done)
	}()
	close(first.messages)
	waitFor(t, "a second session", func() bool { return opened.Load() == 2 && scaler.count() >= 2 })
	cancel()
	<-done
	if !first.closed.Load() || !second.closed.Load() {
		t.Fatal("a session was left open")
	}
}

// GitHub assigns a job to the scale set, and counts it in
// TotalAssignedJobs, only once the listener acquires it. Each job a message
// offers is acquired before the message is scaled and acked.
func TestEachOfferedJobIsAcquired(t *testing.T) {
	session := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	scaler := &recordingScaler{}
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Pool{Repo: repos.Repo{Owner: "acme", Name: "widgets"}, Pool: repos.Pool{Name: "test", MaxRunners: 4}, ScaleSetID: 5},
			func(context.Context) (Session, error) { return session, nil }, scaler, func() {}, slog.New(slog.NewJSONHandler(&logs, nil)), func(int) time.Duration { return time.Millisecond })
		close(done)
	}()
	session.messages <- &scaleset.RunnerScaleSetMessage{
		MessageID:  12,
		Statistics: &scaleset.RunnerScaleSetStatistic{},
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 101}},
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 102}},
		},
	}
	session.messages <- nil
	waitFor(t, "the message handled", func() bool { return scaler.count() >= 2 })
	cancel()
	<-done
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.acquired) != 2 || session.acquired[0] != 101 || session.acquired[1] != 102 {
		t.Fatalf("acquired %v, want 101 and 102", session.acquired)
	}
}

// A session that opens clears the failures before it: the next failure
// waits as briefly as the first did.
func TestAnOpenedSessionResetsTheBackoff(t *testing.T) {
	first := &fakeSession{id: uuid.New(), messages: make(chan *scaleset.RunnerScaleSetMessage)}
	var opened atomic.Int32
	var mu sync.Mutex
	var waits []int
	scaler := &recordingScaler{}
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Pool{Repo: repos.Repo{Owner: "acme", Name: "widgets"}, Pool: repos.Pool{Name: "test", MaxRunners: 4}, ScaleSetID: 5},
			func(context.Context) (Session, error) {
				switch opened.Add(1) {
				case 3:
					return first, nil
				default:
					return nil, errors.New("GitHub answered 500")
				}
			}, scaler, func() {}, slog.New(slog.NewJSONHandler(&logs, nil)),
			func(n int) time.Duration {
				mu.Lock()
				defer mu.Unlock()
				waits = append(waits, n)
				return time.Millisecond
			})
		close(done)
	}()
	waitFor(t, "the third open", func() bool { return scaler.count() >= 1 })
	close(first.messages)
	waitFor(t, "two more failures", func() bool { mu.Lock(); defer mu.Unlock(); return len(waits) >= 4 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if waits[0] != 1 || waits[1] != 2 || waits[2] != 1 || waits[3] != 2 {
		t.Fatalf("waited after failures %v, want 1 2 1 2", waits)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
