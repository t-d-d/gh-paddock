// Package repos reads the list of served repos: which repos the runners
// serve, and each repo's pools. The list is a JSON document the operator
// keeps in Parameter Store in the CI account, never a file in this repo
// (docs/constraints/served-repos.md).
//
//	{"repos": [
//	  {"repo": "owner/name", "pools": [
//	    {"name": "test", "capacity": "spot"},
//	    {"name": "dev", "capacity": "on-demand", "vcpu": 2, "memoryGb": 8, "maxRunners": 10}
//	  ]}
//	]}
//
// A pool's size defaults to 2 vCPU and 8 GB, as GitHub's private-repo
// runners, and its maximum to 10 runners.
package repos

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Capacity is the Fargate capacity a pool's runner tasks run on.
type Capacity string

const (
	Spot     Capacity = "spot"
	OnDemand Capacity = "on-demand"
)

const (
	defaultVCPU       = 2
	defaultMemoryGB   = 8
	defaultMaxRunners = 10
	// A sanity bound on one pool, not a budget: a typo of 1000 should not
	// start a thousand tasks.
	maxMaxRunners = 100
)

// Pool is one of a repo's runner pools: one scale set, one runner group.
type Pool struct {
	Name       string
	Capacity   Capacity
	VCPU       int
	MemoryGB   int
	MaxRunners int
}

// Repo is a served repo and its pools.
type Repo struct {
	Owner string
	Name  string
	Pools []Pool
}

// FullName is the repo's owner/name.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

var unsafeName = regexp.MustCompile(`[^a-z0-9]+`)

// ScaleSetName names the pool's scale set and its runner group, and is the
// label a workflow's runs-on names. Scale sets live in the org, so the name
// carries the repo as well as the pool.
func (r Repo) ScaleSetName(p Pool) string {
	repo := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(r.Name), "-"), "-")
	return "paddock-" + repo + "-" + p.Name
}

// List is the parsed list: the repos to serve, and why any entry was skipped.
type List struct {
	Repos   []Repo
	Skipped []error
}

type document struct {
	Repos []struct {
		Repo  string `json:"repo"`
		Pools []struct {
			Name       string   `json:"name"`
			Capacity   Capacity `json:"capacity"`
			VCPU       *int     `json:"vcpu"`
			MemoryGB   *int     `json:"memoryGb"`
			MaxRunners *int     `json:"maxRunners"`
		} `json:"pools"`
	} `json:"repos"`
}

var (
	repoPattern = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)
	poolPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
)

// Parse reads the list. An empty list serves nothing. A list that is not
// JSON is an error; an entry that is not valid is skipped, with its reason
// in Skipped, and the rest are served.
func Parse(raw string) (List, error) {
	var list List
	if strings.TrimSpace(raw) == "" {
		return list, nil
	}
	var doc document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return list, fmt.Errorf("the repo list is not JSON: %w", err)
	}
	seen := map[string]bool{}
	for i, entry := range doc.Repos {
		m := repoPattern.FindStringSubmatch(entry.Repo)
		if m == nil {
			list.Skipped = append(list.Skipped, fmt.Errorf("entry %d: repo %q is not owner/name", i, entry.Repo))
			continue
		}
		repo := Repo{Owner: m[1], Name: m[2]}
		key := strings.ToLower(repo.FullName())
		if seen[key] {
			list.Skipped = append(list.Skipped, fmt.Errorf("entry %d: repo %s is listed twice", i, repo.FullName()))
			continue
		}
		if len(entry.Pools) == 0 {
			list.Skipped = append(list.Skipped, fmt.Errorf("entry %d: repo %s has no pools", i, repo.FullName()))
			continue
		}
		var problem error
		names := map[string]bool{}
		for _, p := range entry.Pools {
			pool := Pool{
				Name:       p.Name,
				Capacity:   p.Capacity,
				VCPU:       orDefault(p.VCPU, defaultVCPU),
				MemoryGB:   orDefault(p.MemoryGB, defaultMemoryGB),
				MaxRunners: orDefault(p.MaxRunners, defaultMaxRunners),
			}
			if problem = checkPool(pool, names); problem != nil {
				break
			}
			names[pool.Name] = true
			repo.Pools = append(repo.Pools, pool)
		}
		if problem != nil {
			list.Skipped = append(list.Skipped, fmt.Errorf("entry %d: repo %s: %w", i, repo.FullName(), problem))
			continue
		}
		seen[key] = true
		list.Repos = append(list.Repos, repo)
	}
	return list, nil
}

func orDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func checkPool(p Pool, names map[string]bool) error {
	if !poolPattern.MatchString(p.Name) {
		return fmt.Errorf("pool name %q is not lowercase letters, digits and hyphens", p.Name)
	}
	if names[p.Name] {
		return fmt.Errorf("pool %s is listed twice", p.Name)
	}
	if p.Capacity != Spot && p.Capacity != OnDemand {
		return fmt.Errorf("pool %s: capacity %q is neither %q nor %q", p.Name, p.Capacity, Spot, OnDemand)
	}
	if !fargateSize(p.VCPU, p.MemoryGB) {
		return fmt.Errorf("pool %s: size %d vCPU and %d GB is not a Fargate task size", p.Name, p.VCPU, p.MemoryGB)
	}
	if p.MaxRunners < 1 || p.MaxRunners > maxMaxRunners {
		return fmt.Errorf("pool %s: maxRunners %d is not between 1 and %d", p.Name, p.MaxRunners, maxMaxRunners)
	}
	return nil
}

// fargateSize holds the whole-vCPU sizes Fargate offers on Linux.
func fargateSize(vcpu, gb int) bool {
	switch vcpu {
	case 1:
		return gb >= 2 && gb <= 8
	case 2:
		return gb >= 4 && gb <= 16
	case 4:
		return gb >= 8 && gb <= 30
	case 8:
		return gb >= 16 && gb <= 60 && gb%4 == 0
	case 16:
		return gb >= 32 && gb <= 120 && gb%8 == 0
	}
	return false
}
