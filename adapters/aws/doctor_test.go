package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
)

func TestCredentials(t *testing.T) {
	for out, want := range map[string]string{
		// aws CLI 2.3x
		"NAME       : VALUE                    : TYPE             : LOCATION\n" +
			"profile    : <not set>                : None             : None\n" +
			"access_key : ****************LE12     : env              : \n": "env",
		"NAME       : VALUE                    : TYPE             : LOCATION\n" +
			"access_key : <not set>                : None             : None\n": "",
		// older CLIs
		"      Name                    Value             Type    Location\n" +
			"access_key     ****************ABCD              sso    \n": "sso",
		"access_key                <not set>             None    None\n": "",
	} {
		if kind, found := credentials(out); !found || kind != want {
			t.Errorf("credentials(%q) = %q, %v; want %q", out, kind, found, want)
		}
	}
	if _, found := credentials("something else"); found {
		t.Error("found credentials in unrelated output")
	}
}

func TestDoctor(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": {"aws": {"region": "us-east-1", "profile": "prod"}}}`)
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"aws --version":                     {Stdout: "aws-cli/2.33.1 Python/3.13.1 Darwin/25.6.0 exe/arm64\n"},
		"aws configure list --profile prod": {Stdout: "access_key : ****************ABCD : sso : \n"},
		"docker buildx version":             {Stdout: "github.com/docker/buildx v0.33.0 abc\n"},
	}}
	c := New().Doctor(context.Background(), s, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Tools[0].Version != "2.33.1" || c.Tools[0].Login != "prod (sso)" {
		t.Errorf("checkup = %+v", c)
	}
	f.Answers["aws --version"] = exectest.Answer{Stdout: "aws-cli/2.27.0 Python/3.13.1\n"}
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"AWS_PREFLIGHT_VERSION"}) ||
		!strings.Contains(c.Findings[0].Message, "2.32.2 or newer") || !strings.Contains(c.Findings[0].Message, "has 2.27.0") {
		t.Errorf("old aws: %+v", c.Findings)
	}
	f.Answers["aws --version"] = exectest.Answer{Stdout: "aws-cli/2.33.1\n"}
	f.Answers["aws configure list --profile prod"] = exectest.Answer{Stderr: "aws: [ERROR]: The config profile (prod) could not be found", Err: errors.New("exit status 255")}
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"AWS_PREFLIGHT_AUTH"}) {
		t.Errorf("unknown profile: %+v", c.Findings)
	}
	// Output anyship can't read is said so, not passed as a login.
	f.Answers["aws configure list --profile prod"] = exectest.Answer{Stdout: "something new\n"}
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"AWS_PREFLIGHT_AUTH"}) || c.Findings[0].Level != adapter.Warning ||
		!strings.Contains(c.Findings[0].Hint, "aws sts get-caller-identity --profile prod") || c.Tools[0].Login != "" {
		t.Errorf("unreadable credentials: %+v", c)
	}
	f.Answers["aws --version"] = exectest.Missing("aws")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"AWS_PREFLIGHT_CLI"}) {
		t.Errorf("no aws: %+v", c.Findings)
	}
	// Starting the aws CLI is slow; its version is read in one run.
	var versions int
	for _, call := range f.Calls {
		if call.Line == "aws --version" {
			versions++
		}
	}
	if runs := 5; versions != runs {
		t.Errorf("aws --version ran %d times for %d checkups", versions, runs)
	}
}
