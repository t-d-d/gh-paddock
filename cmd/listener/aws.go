package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/runners"
	"github.com/t-d-d/gh-paddock/internal/serve"
)

// awsCloud is the listener's real clients: AWS through the SDK, and GitHub.
// The tests fake the cloud interface instead.
type awsCloud struct {
	ssm *ssm.Client
	ecs *ecs.Client
	s3  *s3.Client
}

func connectAWS(ctx context.Context, region string) (cloud, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &awsCloud{ssm: ssm.NewFromConfig(cfg), ecs: ecs.NewFromConfig(cfg), s3: s3.NewFromConfig(cfg)}, nil
}

func (a *awsCloud) Parameter(ctx context.Context, name string) (string, bool, error) {
	out, err := a.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return aws.ToString(out.Parameter.Value), true, nil
}

func (a *awsCloud) App(key github.AppKey) (app, error) {
	gh, err := github.NewApp(key, github.DefaultAPI, http.DefaultClient)
	if err != nil {
		return nil, err
	}
	return githubApp{gh}, nil
}

func (a *awsCloud) ScaleSets(key github.AppKey, org string, installationID int64) (serve.ScaleSets, error) {
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/" + org,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       strconv.FormatInt(key.AppID, 10),
			InstallationID: installationID,
			PrivateKey:     key.PrivateKey,
		},
		SystemInfo: scaleset.SystemInfo{System: "gh-paddock", Subsystem: "listener"},
	})
	if err != nil {
		return nil, err
	}
	return scaleSets{client}, nil
}

func (a *awsCloud) ECS() runners.ECS { return a.ecs }
func (a *awsCloud) SSM() runners.SSM { return a.ssm }

func (a *awsCloud) Objects() runners.Objects { return a.s3 }

// Presigner signs with the listener's own credentials, so a task's URL
// lasts no longer than they do.
func (a *awsCloud) Presigner() runners.Presigner { return s3.NewPresignClient(a.s3) }

// githubApp gives the App's installation as serve wants it.
type githubApp struct{ *github.App }

func (g githubApp) Installation(ctx context.Context, org string) (serve.Installation, int64, error) {
	inst, err := g.App.Installation(ctx, org)
	if err != nil {
		return nil, 0, err
	}
	return inst, inst.ID, nil
}

// scaleSets gives a scale-set client's sessions as serve wants them.
type scaleSets struct{ *scaleset.Client }

func (s scaleSets) OpenSession(ctx context.Context, scaleSetID int, owner string) (serve.Session, error) {
	return s.Client.MessageSessionClient(ctx, scaleSetID, owner)
}
