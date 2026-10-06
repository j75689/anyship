package gcp

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A cron entry with a path becomes a Cloud Scheduler job that calls the
// service over HTTP. Scheduler jobs carry no labels, so anyship finds its own
// by name: anyship_<spec>_<service>_<n>, where n is the entry's place in the
// service's cron list. Spec and service names can't hold an underscore, which
// keeps the jobs of "shop" apart from those of "shop-admin".
type cronJob struct {
	id       string
	schedule string
	method   string
	path     string
}

func jobPrefix(project string) string { return "anyship_" + project + "_" }

func jobID(project, service string, n int) string {
	return jobPrefix(project) + service + "_" + strconv.Itoa(n)
}

// Cloud Scheduler waits between 15 seconds and 30 minutes for a call.
const (
	minAttemptDeadline = 15
	maxAttemptDeadline = 30 * 60
)

// jobArgs is the gcloud command that creates or updates a job, by verb. The
// call carries an identity token for the account the service runs as, made
// out to the service's URL, and waits as long as the service's own timeout
// allows a request to take.
func jobArgs(verb string, d *planData, sv service, job cronJob, url string) []string {
	deadline := min(max(sv.timeout, minAttemptDeadline), maxAttemptDeadline)
	args := []string{"scheduler", "jobs", verb, "http", job.id, "--location", d.opts.Region,
		"--schedule", job.schedule, "--time-zone", "Etc/UTC",
		"--uri", url + job.path, "--http-method", strings.ToLower(job.method),
		"--attempt-deadline", strconv.Itoa(deadline) + "s",
		"--oidc-service-account-email", sv.account, "--oidc-token-audience", url,
		"--description", fmt.Sprintf("anyship: %s/%s cron %q", d.project, sv.name, job.schedule)}
	if verb == "update" {
		// The spec decides the whole job: drop what was added by hand.
		args = append(args, "--clear-headers", "--clear-message-body")
	}
	return args
}

// applyJobs makes the service's jobs match the spec, and lets their account
// call a service that asks for authentication.
func applyJobs(ctx context.Context, g gcloud, d *planData, sv service, url string) error {
	if sv.internal || d.opts.Private {
		g.env.Logf("$ gcloud run services add-iam-policy-binding %s (run.invoker for %s)", sv.cloudRun, sv.account)
		err := g.run(ctx, nil, io.Discard, "run", "services", "add-iam-policy-binding", sv.cloudRun, "--region", d.opts.Region,
			"--member", "serviceAccount:"+sv.account, "--role", "roles/run.invoker")
		if err != nil {
			return fmt.Errorf("letting %s call %s failed: %w", sv.account, sv.cloudRun, err)
		}
	}
	for _, job := range sv.jobs {
		verb := "create"
		if _, err := g.probe(ctx, "scheduler", "jobs", "describe", job.id, "--location", d.opts.Region); err == nil {
			verb = "update"
		}
		g.env.Logf("$ gcloud scheduler jobs %s http %s (%s %s %s)", verb, job.id, job.schedule, job.method, job.path)
		if err := g.run(ctx, nil, io.Discard, jobArgs(verb, d, sv, job, url)...); err != nil {
			return fmt.Errorf("gcloud scheduler jobs %s %s failed: %w", verb, job.id, err)
		}
	}
	return nil
}

// listJobs names the Cloud Scheduler jobs anyship made for a spec.
func listJobs(ctx context.Context, g gcloud, o Options, project string) ([]string, error) {
	out, err := g.probe(ctx, "scheduler", "jobs", "list", "--location", o.Region, "--format", "value(name.basename())")
	if err != nil {
		return nil, fmt.Errorf("listing Cloud Scheduler jobs in %s/%s failed (%w)", o.Project, o.Region, err)
	}
	var ids []string
	for _, id := range strings.Fields(out) {
		if strings.HasPrefix(id, jobPrefix(project)) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// deleteJobs removes jobs by id, except those keep says to keep, and
// returns how many it removed.
func deleteJobs(ctx context.Context, g gcloud, o Options, ids []string, keep func(id string) bool) (int, error) {
	removed := 0
	for _, id := range ids {
		if keep != nil && keep(id) {
			continue
		}
		g.env.Logf("$ gcloud scheduler jobs delete %s", id)
		if err := g.run(ctx, nil, io.Discard, "scheduler", "jobs", "delete", id, "--location", o.Region); err != nil {
			return removed, fmt.Errorf("deleting Cloud Scheduler job %s failed: %w", id, err)
		}
		removed++
	}
	return removed, nil
}

// removeStaleJobs deletes the spec's jobs that no cron entry asks for any
// more, so an entry taken out of the spec stops firing.
func removeStaleJobs(ctx context.Context, g gcloud, d *planData) error {
	ids, err := listJobs(ctx, g, d.opts, d.project)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, sv := range d.services {
		for _, job := range sv.jobs {
			wanted[job.id] = true
		}
	}
	_, err = deleteJobs(ctx, g, d.opts, ids, func(id string) bool { return wanted[id] })
	return err
}
