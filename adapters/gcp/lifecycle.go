package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

const defaultLogLimit = 100

// cloudRunService is one entry of `gcloud run services list --format json`.
type cloudRunService struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		URL                     string `json:"url"`
		LatestReadyRevisionName string `json:"latestReadyRevisionName"`
		Conditions              []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			Message            string `json:"message"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// image is what the service's latest revision runs.
func (crs cloudRunService) image() string {
	for _, c := range crs.Spec.Template.Spec.Containers {
		return c.Image
	}
	return ""
}

// deployed lists the project's Cloud Run services, found by label.
func deployed(ctx context.Context, g gcloud, o Options, project string) (map[string]cloudRunService, error) {
	// A probe, so gcloud's warning about an unmatched filter stays quiet.
	out, err := g.probe(ctx, "run", "services", "list", "--region", o.Region,
		"--filter", "metadata.labels."+projectLabel+"="+project, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("listing Cloud Run services in %s/%s failed (%w)", o.Project, o.Region, err)
	}
	var list []cloudRunService
	if out != "" {
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return nil, fmt.Errorf("unexpected output from gcloud run services list: %w", err)
		}
	}
	found := map[string]cloudRunService{}
	for _, svc := range list {
		found[svc.Metadata.Name] = svc
	}
	return found, nil
}

func (a *Adapter) Status(ctx context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Status, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.gcp: %w", err)
	}
	found, err := deployed(ctx, newGcloud(env, *o), *o, s.Name)
	if err != nil {
		return nil, err
	}
	st := &adapter.Status{Target: Name, Location: o.Project + "/" + o.Region, Deployed: len(found) > 0}
	for _, name := range s.ServiceNames() {
		ss := adapter.ServiceStatus{Name: name, State: "missing", Desired: 1}
		if crs, ok := found[cloudRunName(s.Name, name)]; ok {
			ss.State, ss.URL, ss.Image = "deploying", crs.Status.URL, crs.image()
			if rev := crs.Status.LatestReadyRevisionName; rev != "" {
				ss.Detail = "revision " + rev
			}
			for _, c := range crs.Status.Conditions {
				if c.Type != "Ready" {
					continue
				}
				ss.Since = c.LastTransitionTime
				switch c.Status {
				case "True":
					ss.State, ss.Running = "running", 1
				case "False":
					ss.State, ss.Health = "failing", "unhealthy"
					if c.Message != "" {
						ss.Detail = c.Message
						ss.Events = []string{c.Message}
					}
				}
			}
		}
		st.Services = append(st.Services, ss)
	}
	return st, nil
}

// Logs reads recent logs with `gcloud run services logs read`. Following
// needs the beta logs tail command, so it's refused with a pointer to it.
func (a *Adapter) Logs(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return fmt.Errorf("spec.targets.gcp: %w", err)
	}
	if opts.Follow {
		configuration := ""
		if o.Configuration != "" {
			configuration = " --configuration " + o.Configuration
		}
		return fmt.Errorf("the gcp target can't follow logs; use `gcloud beta run services logs tail %s --region %s --project %s%s`",
			cloudRunName(s.Name, firstOr(opts.Service, s.ServiceNames())), o.Region, o.Project, configuration)
	}
	if opts.Since != "" {
		if _, err := time.ParseDuration(opts.Since); err != nil {
			return errors.New("the gcp target takes --since as a duration such as 10m or 2h")
		}
	}
	limit := opts.Tail
	if limit == 0 {
		limit = defaultLogLimit
	}
	names := s.ServiceNames()
	if opts.Service != "" {
		names = []string{opts.Service}
	}
	g := newGcloud(env, *o)
	for _, name := range names {
		args := []string{"run", "services", "logs", "read", cloudRunName(s.Name, name), "--region", o.Region, "--limit", strconv.Itoa(limit)}
		if opts.Since != "" {
			args = append(args, "--freshness", opts.Since)
		}
		if len(names) > 1 {
			env.Logf("== %s", name)
		}
		if err := g.run(ctx, nil, nil, args...); err != nil {
			return fmt.Errorf("reading logs for %s failed (%w); has it been deployed with `anyship apply -t gcp`?", name, err)
		}
	}
	return nil
}

func (a *Adapter) DestroySummary(s *spec.Spec, opts adapter.DestroyOptions) ([]string, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.gcp: %w", err)
	}
	var names []string
	for _, name := range s.ServiceNames() {
		names = append(names, cloudRunName(s.Name, name))
	}
	lines := []string{fmt.Sprintf("Delete the Cloud Run services %s in %s/%s.", strings.Join(names, ", "), o.Project, o.Region)}
	if slices.ContainsFunc(s.ServiceNames(), func(name string) bool { return len(s.Services[name].Cron) > 0 }) {
		lines = append(lines, fmt.Sprintf("Delete the Cloud Scheduler jobs %s* in %s/%s.", jobPrefix(s.Name), o.Project, o.Region))
	}
	secrets := secretIDs(s)
	switch {
	case opts.Volumes && len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the Secret Manager secrets %s. Their values cannot be recovered.", strings.Join(secrets, ", ")))
	case len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("Keep the Secret Manager secrets %s; destroy --volumes deletes them.", strings.Join(secrets, ", ")))
	}
	return append(lines, "Keep the images in Artifact Registry."), nil
}

func (a *Adapter) Destroy(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.gcp: %w", err)
	}
	g := newGcloud(env, *o)
	found, err := deployed(ctx, g, *o, s.Name)
	if err != nil {
		return nil, err
	}
	removed := 0
	for _, name := range s.ServiceNames() {
		crName := cloudRunName(s.Name, name)
		if _, ok := found[crName]; !ok {
			continue
		}
		env.Logf("$ gcloud run services delete %s", crName)
		if err := g.run(ctx, nil, nil, "run", "services", "delete", crName, "--region", o.Region); err != nil {
			return &adapter.Result{Messages: []string{fmt.Sprintf("deleting %s failed: %v", crName, err)}}, nil
		}
		removed++
	}
	// The jobs are found by name, also those of cron entries the spec has
	// dropped since. A project without the Cloud Scheduler API has none, and
	// the list fails there; that only matters if the spec has cron entries.
	var notes []string
	jobs, err := listJobs(ctx, g, *o, s.Name)
	switch {
	case err == nil:
		n, err := deleteJobs(ctx, g, *o, jobs, nil)
		removed += n
		if err != nil {
			return &adapter.Result{Messages: []string{err.Error()}}, nil
		}
	case slices.ContainsFunc(s.ServiceNames(), func(name string) bool { return len(s.Services[name].Cron) > 0 }):
		notes = append(notes, fmt.Sprintf("Cloud Scheduler jobs were not removed: %v. Look for jobs named %s* in %s/%s.", err, jobPrefix(s.Name), o.Project, o.Region))
	}
	if opts.Volumes {
		for _, id := range secretIDs(s) {
			if _, err := g.probe(ctx, "secrets", "describe", id); err != nil {
				continue
			}
			env.Logf("$ gcloud secrets delete %s", id)
			if err := g.run(ctx, nil, nil, "secrets", "delete", id); err != nil {
				return &adapter.Result{Messages: []string{fmt.Sprintf("deleting secret %s failed: %v", id, err)}}, nil
			}
		}
	}
	if removed == 0 && !opts.Volumes {
		return &adapter.Result{OK: true, Messages: append([]string{fmt.Sprintf("Nothing to remove: %s has no Cloud Run services in %s/%s.", s.Name, o.Project, o.Region)}, notes...)}, nil
	}
	return &adapter.Result{OK: true, Messages: append([]string{fmt.Sprintf("Removed %s from Cloud Run in %s/%s.", s.Name, o.Project, o.Region)}, notes...)}, nil
}

func secretIDs(s *spec.Spec) []string {
	var ids []string
	for _, name := range sortedKeys(s.Secrets) {
		ids = append(ids, secretID(s.Name, name))
	}
	return ids
}

func firstOr(value string, fallback []string) string {
	if value != "" || len(fallback) == 0 {
		return value
	}
	return fallback[0]
}
