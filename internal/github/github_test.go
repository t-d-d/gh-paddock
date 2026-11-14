package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func pkcs1(t *testing.T) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key(t))}))
}

func keyJSON(t *testing.T, id any) string {
	raw, err := json.Marshal(map[string]any{"appId": id, "privateKey": pkcs1(t)})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTheAppKeyParsesFromTheParametersFormat(t *testing.T) {
	k, err := ParseAppKey(keyJSON(t, 42))
	if err != nil {
		t.Fatal(err)
	}
	if k.AppID != 42 || k.PrivateKey != pkcs1(t) {
		t.Fatalf("parsed %d", k.AppID)
	}
}

func TestABadAppKeyIsAnErrorThatNeverEchoesTheKey(t *testing.T) {
	cases := map[string]string{
		"not json":         `{"appId": `,
		"id as a string":   keyJSON(t, "42"),
		"no id":            keyJSON(t, 0),
		"no key":           `{"appId": 42}`,
		"key is not a PEM": `{"appId": 42, "privateKey": "secret-material"}`,
	}
	for name, raw := range cases {
		_, err := ParseAppKey(raw)
		if err == nil {
			t.Errorf("%s: parsed", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-material") || strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("%s: the error carries the key: %v", name, err)
		}
	}
}

// fakeGitHub answers the REST calls the App makes, and records them.
type fakeGitHub struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
	// runner groups by name
	groups    map[string]map[string]any
	groupRepo map[int][]int
	nextGroup int
	repos     map[string]map[string]any
	installs  map[string]int64
	tokens    int
}

func newFake(t *testing.T) (*fakeGitHub, *App) {
	f := &fakeGitHub{t: t, groups: map[string]map[string]any{}, groupRepo: map[int][]int{}, nextGroup: 2,
		repos:    map[string]map[string]any{"acme/widgets": {"id": 7, "private": true}, "acme/open": {"id": 8, "private": false}},
		installs: map[string]int64{"acme": 99}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	k, err := ParseAppKey(keyJSON(t, 42))
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(k, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, app
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	auth := r.Header.Get("Authorization")
	appCall := r.URL.Path == "/app" || strings.HasPrefix(r.URL.Path, "/app/") || strings.HasSuffix(r.URL.Path, "/installation")
	if appCall {
		f.checkJWT(w, auth)
	} else if auth != "token installation-token" {
		http.Error(w, "bad token "+auth, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/app":
		reply(200, map[string]any{"id": 42, "slug": "paddock"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/installation"):
		org := strings.Split(r.URL.Path, "/")[2]
		if id, ok := f.installs[org]; ok {
			reply(200, map[string]any{"id": id})
		} else {
			reply(404, map[string]any{"message": "Not Found"})
		}
	case r.Method == "POST" && r.URL.Path == "/app/installations/99/access_tokens":
		f.tokens++
		reply(201, map[string]any{"token": "installation-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/"):
		name := strings.TrimPrefix(r.URL.Path, "/repos/")
		if repo, ok := f.repos[name]; ok {
			reply(200, repo)
		} else {
			reply(404, map[string]any{"message": "Not Found"})
		}
	case r.Method == "GET" && r.URL.Path == "/orgs/acme/actions/runner-groups":
		var list []any
		for _, g := range f.groups {
			list = append(list, g)
		}
		reply(200, map[string]any{"total_count": len(list), "runner_groups": list})
	case r.Method == "POST" && r.URL.Path == "/orgs/acme/actions/runner-groups":
		id := f.nextGroup
		f.nextGroup++
		g := map[string]any{"id": id, "name": in["name"], "visibility": in["visibility"], "allows_public_repositories": in["allows_public_repositories"]}
		f.groups[in["name"].(string)] = g
		for _, r := range in["selected_repository_ids"].([]any) {
			f.groupRepo[id] = append(f.groupRepo[id], int(r.(float64)))
		}
		reply(201, g)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/orgs/acme/actions/runner-groups/"):
		for _, g := range f.groups {
			if fmt.Sprint(g["id"]) == strings.TrimPrefix(r.URL.Path, "/orgs/acme/actions/runner-groups/") {
				g["visibility"] = in["visibility"]
				g["allows_public_repositories"] = in["allows_public_repositories"]
				reply(200, g)
				return
			}
		}
		reply(404, map[string]any{"message": "Not Found"})
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/repositories"):
		var id int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/orgs/acme/actions/runner-groups/"), "%d", &id)
		f.groupRepo[id] = nil
		for _, r := range in["selected_repository_ids"].([]any) {
			f.groupRepo[id] = append(f.groupRepo[id], int(r.(float64)))
		}
		w.WriteHeader(204)
	default:
		reply(404, map[string]any{"message": "no route " + r.Method + " " + r.URL.Path})
	}
}

func (f *fakeGitHub) checkJWT(w http.ResponseWriter, auth string) {
	raw := strings.TrimPrefix(auth, "Bearer ")
	tok, err := jwt.ParseWithClaims(raw, &jwt.RegisteredClaims{}, func(*jwt.Token) (any, error) { return &testKey.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		f.t.Errorf("an App call carried a bad JWT: %v", err)
		return
	}
	if iss, _ := tok.Claims.GetIssuer(); iss != "42" {
		f.t.Errorf("the JWT's issuer is %q, not the App's id", iss)
	}
}

func TestCheckReachesGitHubAsTheApp(t *testing.T) {
	f, app := newFake(t)
	if err := app.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "GET /app" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestAnOrgWithoutTheAppCannotBeServed(t *testing.T) {
	_, app := newFake(t)
	_, err := app.Installation(context.Background(), "elsewhere")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err %v", err)
	}
}

func TestARepoGetsARunnerGroupOpenToItAloneWithPublicReposOff(t *testing.T) {
	f, app := newFake(t)
	ctx := context.Background()
	inst, err := app.Installation(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := inst.Repo(ctx, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != 7 || !repo.Private {
		t.Fatalf("repo %+v", repo)
	}
	if err := inst.EnsureRunnerGroup(ctx, "acme", "paddock-widgets-test", repo.ID); err != nil {
		t.Fatal(err)
	}
	g := f.groups["paddock-widgets-test"]
	if g["visibility"] != "selected" || g["allows_public_repositories"] != false {
		t.Fatalf("group %v", g)
	}
	if got := f.groupRepo[g["id"].(int)]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("group open to %v", got)
	}
	// Run again, against a group someone widened by hand: it is narrowed
	// back to the one repo, with public repos off.
	g["visibility"] = "all"
	g["allows_public_repositories"] = true
	f.groupRepo[g["id"].(int)] = []int{7, 8}
	if err := inst.EnsureRunnerGroup(ctx, "acme", "paddock-widgets-test", repo.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.groups) != 1 || g["visibility"] != "selected" || g["allows_public_repositories"] != false {
		t.Fatalf("groups %v", f.groups)
	}
	if got := f.groupRepo[g["id"].(int)]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("group open to %v", got)
	}
	if f.tokens != 1 {
		t.Fatalf("minted %d installation tokens, want one reused", f.tokens)
	}
}

func TestAMissingRepoIsAnError(t *testing.T) {
	_, app := newFake(t)
	inst, err := app.Installation(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Repo(context.Background(), "acme", "gone"); err == nil {
		t.Fatal("a missing repo was found")
	}
}
