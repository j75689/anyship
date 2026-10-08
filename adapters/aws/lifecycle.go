package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// ecsService is one entry of `aws ecs describe-services`.
type ecsService struct {
	ServiceName  string `json:"serviceName"`
	ServiceArn   string `json:"serviceArn"`
	Status       string `json:"status"`
	DesiredCount int    `json:"desiredCount"`
	RunningCount int    `json:"runningCount"`
	Deployments  []struct {
		Status             string `json:"status"`
		RolloutState       string `json:"rolloutState"`
		RolloutStateReason string `json:"rolloutStateReason"`
		CreatedAt          string `json:"createdAt"`
		TaskDefinition     string `json:"taskDefinition"`
	} `json:"deployments"`
}

// taskImage is the image the task definition runs.
func taskImage(ctx context.Context, c cli, taskDefinition string) string {
	var out struct {
		TaskDefinition struct {
			ContainerDefinitions []struct {
				Image string `json:"image"`
			} `json:"containerDefinitions"`
		} `json:"taskDefinition"`
	}
	if taskDefinition == "" || c.json(ctx, &out, "ecs", "describe-task-definition", "--task-definition", taskDefinition) != nil {
		return ""
	}
	for _, cd := range out.TaskDefinition.ContainerDefinitions {
		return cd.Image
	}
	return ""
}

// rfc3339 normalises the timestamps the aws CLI prints, which carry
// fractions and a numeric zone, to RFC 3339 in UTC; anything else is kept.
func rfc3339(s string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000000-07:00", "2006-01-02T15:04:05-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// describeServices looks up services by name. Services that don't exist, or
// were deleted (INACTIVE), are left out.
func describeServices(ctx context.Context, c cli, o Options, names []string) (map[string]*ecsService, error) {
	found := map[string]*ecsService{}
	for start := 0; start < len(names); start += 10 { // the API takes ten at a time
		batch := names[start:min(start+10, len(names))]
		var out struct {
			Services []*ecsService `json:"services"`
		}
		args := append([]string{"ecs", "describe-services", "--cluster", o.cluster(), "--services"}, batch...)
		if err := c.json(ctx, &out, args...); err != nil {
			return nil, fmt.Errorf("listing services in cluster %s (%s) failed (%w)", o.cluster(), o.Region, err)
		}
		for _, svc := range out.Services {
			if svc.Status != "INACTIVE" {
				found[svc.ServiceName] = svc
			}
		}
	}
	return found, nil
}

// expressService is the part of `aws ecs describe-express-gateway-service`
// anyship reads.
type expressService struct {
	ActiveConfigurations []struct {
		IngressPaths []struct {
			AccessType string `json:"accessType"`
			Endpoint   string `json:"endpoint"`
		} `json:"ingressPaths"`
		PrimaryContainer struct {
			AwsLogsConfiguration *struct {
				LogGroup        string `json:"logGroup"`
				LogStreamPrefix string `json:"logStreamPrefix"`
			} `json:"awsLogsConfiguration"`
		} `json:"primaryContainer"`
	} `json:"activeConfigurations"`
}

func describeExpress(ctx context.Context, c cli, arn string) (*expressService, error) {
	var out struct {
		Service expressService `json:"service"`
	}
	if err := c.json(ctx, &out, "ecs", "describe-express-gateway-service", "--service-arn", arn); err != nil {
		return nil, err
	}
	return &out.Service, nil
}

// url is the service's endpoint, preferring a public one.
func (e *expressService) url() string {
	var endpoint string
	for _, cfg := range e.ActiveConfigurations {
		for _, p := range cfg.IngressPaths {
			if endpoint == "" || p.AccessType == "PUBLIC" {
				endpoint = p.Endpoint
			}
		}
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	return endpoint
}

func specNames(s *spec.Spec) []string {
	var names []string
	for _, name := range s.ServiceNames() {
		names = append(names, expressName(s.Name, name))
	}
	return names
}

func (a *Adapter) Status(ctx context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Status, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.aws: %w", err)
	}
	c := cli{env: env, opts: *o}
	found, err := describeServices(ctx, c, *o, specNames(s))
	if err != nil {
		return nil, err
	}
	st := &adapter.Status{Target: Name, Location: o.Region + "/" + o.cluster(), Deployed: len(found) > 0}
	for _, name := range s.ServiceNames() {
		ss := adapter.ServiceStatus{Name: name, State: "missing", Desired: s.Services[name].Replicas}
		if svc := found[expressName(s.Name, name)]; svc != nil {
			ss.Running, ss.Desired = svc.RunningCount, svc.DesiredCount
			ss.State = serviceState(svc)
			for _, dep := range svc.Deployments {
				if dep.Status != "PRIMARY" {
					continue
				}
				ss.Since, ss.Image = rfc3339(dep.CreatedAt), taskImage(ctx, c, dep.TaskDefinition)
				if dep.RolloutState == "FAILED" {
					ss.Detail = dep.RolloutStateReason
				}
				if dep.RolloutStateReason != "" && dep.RolloutState != "COMPLETED" {
					ss.Events = []string{dep.RolloutStateReason}
				}
			}
			if ex, err := describeExpress(ctx, c, svc.ServiceArn); err == nil {
				ss.URL = ex.url()
			}
		}
		st.Services = append(st.Services, ss)
	}
	return st, nil
}

func serviceState(svc *ecsService) string {
	if svc.Status == "DRAINING" {
		return "deleting"
	}
	for _, dep := range svc.Deployments {
		if dep.Status != "PRIMARY" {
			continue
		}
		switch dep.RolloutState {
		case "FAILED":
			return "failing"
		case "IN_PROGRESS":
			return "deploying"
		}
	}
	if svc.RunningCount < svc.DesiredCount {
		return "starting"
	}
	return "running"
}

// Logs reads CloudWatch Logs with `aws logs tail`, from the log group Express
// Mode set up for each service.
func (a *Adapter) Logs(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return fmt.Errorf("spec.targets.aws: %w", err)
	}
	names := s.ServiceNames()
	if opts.Service != "" {
		names = []string{opts.Service}
	}
	if opts.Follow && len(names) > 1 {
		return errors.New("the aws target follows one service at a time; name it, e.g. `anyship logs -t aws web -f`")
	}
	if opts.Tail != 0 {
		env.Logf("note: the aws target selects logs by time; -n is ignored, use --since")
	}
	c := cli{env: env, opts: *o}
	var express []string
	for _, name := range names {
		express = append(express, expressName(s.Name, name))
	}
	found, err := describeServices(ctx, c, *o, express)
	if err != nil {
		return err
	}
	for i, name := range names {
		svc := found[express[i]]
		if svc == nil {
			return fmt.Errorf("%s isn't deployed; deploy it with `anyship apply -t aws`", name)
		}
		ex, err := describeExpress(ctx, c, svc.ServiceArn)
		if err != nil {
			return fmt.Errorf("reading %s's settings failed: %w", name, err)
		}
		if len(ex.ActiveConfigurations) == 0 || ex.ActiveConfigurations[0].PrimaryContainer.AwsLogsConfiguration == nil {
			return fmt.Errorf("%s has no CloudWatch Logs configuration", name)
		}
		logs := ex.ActiveConfigurations[0].PrimaryContainer.AwsLogsConfiguration
		args := []string{"logs", "tail", logs.LogGroup, "--format", "short"}
		if logs.LogStreamPrefix != "" {
			args = append(args, "--log-stream-name-prefix", logs.LogStreamPrefix)
		}
		if opts.Since != "" {
			args = append(args, "--since", opts.Since)
		}
		if opts.Follow {
			args = append(args, "--follow")
		}
		if len(names) > 1 {
			env.Logf("== %s", name)
		}
		if err := c.run(ctx, nil, nil, args...); err != nil {
			return fmt.Errorf("reading logs for %s failed: %w", name, err)
		}
	}
	return nil
}

func (a *Adapter) DestroySummary(s *spec.Spec, opts adapter.DestroyOptions) ([]string, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.aws: %w", err)
	}
	lines := []string{fmt.Sprintf("Delete the ECS Express Mode services %s in cluster %s (%s), with their load balancers and autoscaling.",
		strings.Join(specNames(s), ", "), o.cluster(), o.Region)}
	secrets := secretIDs(s)
	switch {
	case opts.Volumes && len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the Secrets Manager secrets %s at once, without a recovery window. Their values cannot be recovered.", strings.Join(secrets, ", ")))
	case len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("Keep the Secrets Manager secrets %s; destroy --volumes deletes them.", strings.Join(secrets, ", ")))
	}
	return append(lines, "Keep the images in ECR, the cluster, the IAM roles and the CloudWatch log groups."), nil
}

func (a *Adapter) Destroy(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.aws: %w", err)
	}
	c := cli{env: env, opts: *o}
	found, err := describeServices(ctx, c, *o, specNames(s))
	if err != nil {
		return nil, err
	}
	removed := 0
	// Services an earlier destroy is still deleting. Their load balancers
	// bill until that finishes, so they are reported, never passed over as
	// if nothing were there.
	var draining []string
	for _, name := range specNames(s) {
		svc := found[name]
		if svc == nil {
			continue
		}
		if svc.Status == "DRAINING" {
			draining = append(draining, name)
			continue
		}
		env.Logf("$ aws ecs delete-express-gateway-service %s", name)
		if err := c.run(ctx, nil, io.Discard, "ecs", "delete-express-gateway-service", "--service-arn", svc.ServiceArn); err != nil {
			return &adapter.Result{Messages: []string{fmt.Sprintf("deleting %s failed: %v", name, err)}}, nil
		}
		removed++
	}
	if opts.Volumes {
		for _, id := range secretIDs(s) {
			if c.run(ctx, nil, io.Discard, "secretsmanager", "describe-secret", "--secret-id", id) != nil {
				continue
			}
			env.Logf("$ aws secretsmanager delete-secret %s", id)
			if err := c.run(ctx, nil, io.Discard, "secretsmanager", "delete-secret", "--secret-id", id, "--force-delete-without-recovery"); err != nil {
				return &adapter.Result{Messages: []string{fmt.Sprintf("deleting secret %s failed: %v", id, err)}}, nil
			}
		}
	}
	if removed == 0 && len(draining) == 0 && !opts.Volumes {
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Nothing to remove: %s has no services in cluster %s (%s).", s.Name, o.cluster(), o.Region)}}, nil
	}
	var messages []string
	if removed > 0 || len(draining) == 0 {
		messages = append(messages, fmt.Sprintf("Removed %s from ECS in %s; load balancers drain over a few minutes.", s.Name, o.Region))
	}
	if len(draining) > 0 {
		messages = append(messages, fmt.Sprintf("Still being deleted by an earlier destroy: %s. Their load balancers bill until that finishes, usually within a few minutes; `anyship status -t aws` shows when they are gone.",
			strings.Join(draining, ", ")))
	}
	return &adapter.Result{OK: true, Messages: messages}, nil
}

func secretIDs(s *spec.Spec) []string {
	var ids []string
	for _, name := range sortedKeys(s.Secrets) {
		ids = append(ids, secretName(s.Name, name))
	}
	return ids
}
