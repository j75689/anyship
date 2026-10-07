package gcp

import (
	"context"
	"fmt"
	"io"
	"slices"
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
// call goes to url, the form ${services.<name>.url} expands to, and carries
// an identity token for the account the service runs as with that same url
// as audience: Cloud Run takes either of a service's URLs as audience, but
// an app compares the audience literally, so it has to be the one the spec
// can tell it. The call waits as long as the service's own timeout allows a
// request to take.
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

// jobService is the service a job id was made for.
func jobService(id, project string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(id, jobPrefix(project)), "_")
	return name
}

// jobsOf picks one service's jobs out of a spec's.
func jobsOf(ids []string, project, service string) []string {
	var out []string
	for _, id := range ids {
		if jobService(id, project) == service {
			out = append(out, id)
		}
	}
	return out
}

// orphanJobs picks the jobs of services the spec no longer has.
func orphanJobs(ids []string, d *planData) []string {
	var out []string
	for _, id := range ids {
		name := jobService(id, d.project)
		if !slices.ContainsFunc(d.services, func(sv service) bool { return sv.name == name }) {
			out = append(out, id)
		}
	}
	return out
}

// wantsJob reports whether a cron entry of the service asks for the job.
func (sv service) wantsJob(id string) bool {
	return slices.ContainsFunc(sv.jobs, func(job cronJob) bool { return job.id == id })
}
