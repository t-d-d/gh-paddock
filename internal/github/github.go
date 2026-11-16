// Package github is the listener's GitHub App: the REST calls the scale-set
// client does not make. It checks the App's key, finds the App's
// installation on an org, reads a repo, and keeps each pool's runner group
// open to its one repo with public repos off. It re-runs a job a Spot
// reclaim killed, once the job's workflow run completes.
//
// It holds the App key, which only the listener does
// (docs/constraints/runner-isolation.md), and never prints it.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// AppKey is the App's id and private key, as the operator writes them to
// the parameter /paddock/github-app.
type AppKey struct {
	AppID      int64
	PrivateKey string
}

// ParseAppKey reads the parameter's value: one JSON object, appId a JSON
// number and privateKey the PKCS#1 PEM. An error never quotes the value.
func ParseAppKey(raw string) (AppKey, error) {
	var doc struct {
		AppID      json.RawMessage `json:"appId"`
		PrivateKey string          `json:"privateKey"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return AppKey{}, errors.New("the App key's parameter is not a JSON object")
	}
	id, err := strconv.ParseInt(string(doc.AppID), 10, 64)
	if err != nil || id <= 0 {
		return AppKey{}, errors.New("the App key's appId is not a positive JSON number")
	}
	if doc.PrivateKey == "" {
		return AppKey{}, errors.New("the App key has no privateKey")
	}
	if _, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(doc.PrivateKey)); err != nil {
		return AppKey{}, errors.New("the App key's privateKey is not an RSA private key in PEM form")
	}
	return AppKey{AppID: id, PrivateKey: doc.PrivateKey}, nil
}

// App calls GitHub's REST API as the App.
type App struct {
	id   int64
	key  *rsa.PrivateKey
	api  string
	http *http.Client
}

// NewApp makes an App that calls the API at api.
func NewApp(k AppKey, api string, client *http.Client) (*App, error) {
	priv, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(k.PrivateKey))
	if err != nil {
		return nil, errors.New("the App key's privateKey is not an RSA private key in PEM form")
	}
	return &App{id: k.AppID, key: priv, api: strings.TrimRight(api, "/"), http: client}, nil
}

func (a *App) jwt() (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		// A minute back, for clock drift; ten minutes is GitHub's limit.
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    strconv.FormatInt(a.id, 10),
	}).SignedString(a.key)
}

// StatusError is a reply GitHub gave with an unexpected status.
type StatusError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", e.Method, e.Path, e.Status, e.Message)
}

func (a *App) do(ctx context.Context, auth, method, path string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != want {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &msg)
		return &StatusError{Method: method, Path: path, Status: resp.StatusCode, Message: msg.Message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}

func (a *App) asApp(ctx context.Context, method, path string, in, out any, want int) error {
	tok, err := a.jwt()
	if err != nil {
		return err
	}
	return a.do(ctx, "Bearer "+tok, method, path, in, out, want)
}

// Check calls GitHub as the App: it passes only if GitHub accepts the key.
func (a *App) Check(ctx context.Context) error {
	return a.asApp(ctx, http.MethodGet, "/app", nil, nil, http.StatusOK)
}

// Installation is the App's installation on one org.
type Installation struct {
	app *App
	ID  int64

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Installation finds the App's installation on org. An org without the App
// is an error that says so.
func (a *App) Installation(ctx context.Context, org string) (*Installation, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.asApp(ctx, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/installation", nil, &out, http.StatusOK)
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return nil, fmt.Errorf("the App is not installed on %s", org)
	}
	if err != nil {
		return nil, err
	}
	return &Installation{app: a, ID: out.ID}, nil
}

func (i *Installation) auth(ctx context.Context) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.token != "" && time.Until(i.expires) > 5*time.Minute {
		return "token " + i.token, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", i.ID)
	if err := i.app.asApp(ctx, http.MethodPost, path, nil, &out, http.StatusCreated); err != nil {
		return "", err
	}
	i.token, i.expires = out.Token, out.ExpiresAt
	return "token " + i.token, nil
}

func (i *Installation) do(ctx context.Context, method, path string, in, out any, want int) error {
	auth, err := i.auth(ctx)
	if err != nil {
		return err
	}
	return i.app.do(ctx, auth, method, path, in, out, want)
}

// Repo is what the listener needs of a repo.
type Repo struct {
	ID      int64 `json:"id"`
	Private bool  `json:"private"`
}

// Repo reads owner/name.
func (i *Installation) Repo(ctx context.Context, owner, name string) (Repo, error) {
	var out Repo
	err := i.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &out, http.StatusOK)
	return out, err
}

type runnerGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// EnsureRunnerGroup makes the org's runner group named name open to the one
// repo repoID, with public repos off, creating it if missing and narrowing
// it back if someone widened it.
func (i *Installation) EnsureRunnerGroup(ctx context.Context, org, name string, repoID int64) error {
	base := "/orgs/" + url.PathEscape(org) + "/actions/runner-groups"
	settings := map[string]any{"visibility": "selected", "allows_public_repositories": false}
	group, err := i.findRunnerGroup(ctx, base, name)
	if err != nil {
		return err
	}
	if group == nil {
		create := map[string]any{"name": name, "selected_repository_ids": []int64{repoID}}
		for k, v := range settings {
			create[k] = v
		}
		return i.do(ctx, http.MethodPost, base, create, nil, http.StatusCreated)
	}
	path := fmt.Sprintf("%s/%d", base, group.ID)
	if err := i.do(ctx, http.MethodPatch, path, settings, nil, http.StatusOK); err != nil {
		return err
	}
	return i.do(ctx, http.MethodPut, path+"/repositories", map[string]any{"selected_repository_ids": []int64{repoID}}, nil, http.StatusNoContent)
}

func (i *Installation) findRunnerGroup(ctx context.Context, base, name string) (*runnerGroup, error) {
	var found *runnerGroup
	err := i.eachRunnerGroup(ctx, base, func(g runnerGroup) bool {
		if g.Name == name {
			found = &g
			return false
		}
		return true
	})
	return found, err
}

// DeleteRunnerGroup deletes the org's runner group named name, once a
// removed pool's scale set has gone. A group already gone is no error. A
// group that still holds a runner is left, and is an error: GitHub would
// move the runner to the org's default group, open to every repo
// (docs/constraints/pool-per-repo.md).
func (i *Installation) DeleteRunnerGroup(ctx context.Context, org, name string) error {
	base := "/orgs/" + url.PathEscape(org) + "/actions/runner-groups"
	group, err := i.findRunnerGroup(ctx, base, name)
	if err != nil || group == nil {
		return err
	}
	path := fmt.Sprintf("%s/%d", base, group.ID)
	var runners struct {
		Total int `json:"total_count"`
	}
	if err := i.do(ctx, http.MethodGet, path+"/runners?per_page=1", nil, &runners, http.StatusOK); err != nil {
		return err
	}
	if runners.Total > 0 {
		return fmt.Errorf("the runner group still holds %d runners", runners.Total)
	}
	return i.do(ctx, http.MethodDelete, path, nil, nil, http.StatusNoContent)
}

// RunnerGroups names every runner group of org.
func (i *Installation) RunnerGroups(ctx context.Context, org string) ([]string, error) {
	var names []string
	err := i.eachRunnerGroup(ctx, "/orgs/"+url.PathEscape(org)+"/actions/runner-groups", func(g runnerGroup) bool {
		names = append(names, g.Name)
		return true
	})
	return names, err
}

// eachRunnerGroup calls more with each of the org's runner groups, a page at
// a time, until more returns false.
func (i *Installation) eachRunnerGroup(ctx context.Context, base string, more func(runnerGroup) bool) error {
	for page := 1; ; page++ {
		var out struct {
			Total  int           `json:"total_count"`
			Groups []runnerGroup `json:"runner_groups"`
		}
		if err := i.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), nil, &out, http.StatusOK); err != nil {
			return err
		}
		for _, g := range out.Groups {
			if !more(g) {
				return nil
			}
		}
		if len(out.Groups) < 100 {
			return nil
		}
	}
}

// Run is what the listener needs of a workflow run: whether it has
// completed, and its latest attempt.
type Run struct {
	Status  string `json:"status"`
	Attempt int    `json:"run_attempt"`
}

// Run reads a workflow run of owner/name.
func (i *Installation) Run(ctx context.Context, owner, name string, runID int64) (Run, error) {
	var out Run
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(name), runID)
	err := i.do(ctx, http.MethodGet, path, nil, &out, http.StatusOK)
	return out, err
}

// Job is one job of a workflow run's attempt.
type Job struct {
	ID          int64     `json:"id"`
	RunnerName  string    `json:"runner_name"`
	Conclusion  string    `json:"conclusion"`
	CompletedAt time.Time `json:"completed_at"`
}

// RunJobs reads every job of one attempt of a workflow run.
func (i *Installation) RunJobs(ctx context.Context, owner, name string, runID int64, attempt int) ([]Job, error) {
	base := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d/jobs", url.PathEscape(owner), url.PathEscape(name), runID, attempt)
	var jobs []Job
	for page := 1; ; page++ {
		var out struct {
			Total int   `json:"total_count"`
			Jobs  []Job `json:"jobs"`
		}
		if err := i.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		jobs = append(jobs, out.Jobs...)
		if len(out.Jobs) == 0 || len(jobs) >= out.Total {
			return jobs, nil
		}
	}
}

// RerunJob re-runs one job of a completed workflow run, as a new attempt.
// It needs the App's Actions write permission, which is held for this
// alone (docs/constraints/runner-isolation.md).
func (i *Installation) RerunJob(ctx context.Context, owner, name string, jobID int64) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/rerun", url.PathEscape(owner), url.PathEscape(name), jobID)
	return i.do(ctx, http.MethodPost, path, map[string]any{}, nil, http.StatusCreated)
}
