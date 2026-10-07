package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/image"
	"github.com/j75689/anyship/spec"
)

// rolloutTimeout is how long apply waits for a Deployment's new pods to be
// ready before it reports the rollout as failed. The rollout itself goes on
// in the cluster, with the previous pods still serving.
const rolloutTimeout = 5 * time.Minute

// kubectl runs kubectl commands in one context and namespace.
type kubectl struct {
	env       *adapter.Env
	context   string
	namespace string
}

func newKubectl(env *adapter.Env, o Options) kubectl {
	return kubectl{env: env, context: o.Context, namespace: o.Namespace}
}

func (k kubectl) args(args ...string) []string {
	var all []string
	if k.context != "" {
		all = append(all, "--context", k.context)
	}
	all = append(all, "--namespace", k.namespace)
	return append(all, args...)
}

func (k kubectl) options(in io.Reader, out, errOut io.Writer) adapter.ExecOptions {
	return adapter.ExecOptions{Dir: k.env.Dir, Stdin: in, Stdout: out, Stderr: errOut}
}

// run executes kubectl with the terminal's error output; stdout is captured
// when out is set and stdin is fed from in.
func (k kubectl) run(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	return k.env.Exec(ctx, k.options(in, out, nil), "kubectl", k.args(args...)...)
}

// probe runs a check whose failure anyship reports itself. kubectl's error
// output is kept out of the terminal and folded into the error instead.
func (k kubectl) probe(ctx context.Context, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	if err := k.env.Exec(ctx, k.options(nil, &out, &errOut), "kubectl", k.args(args...)...); err != nil {
		if msg := kubectlError(errOut.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// kubectlError picks the message out of kubectl's error output: the last
// line, without its "error: " prefix.
func kubectlError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return strings.TrimPrefix(last, "error: ")
}

// Apply runs the preflight checks, then sets secrets, builds and pushes
// images, applies every object and waits for each rollout. With env.DryRun
// it stops after the checks.
func (a *Adapter) Apply(ctx context.Context, plan *adapter.Plan, _ *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	if adapter.HasErrors(plan.Findings) {
		return &adapter.Result{Messages: []string{"The plan has errors; fix them and plan again."}}, nil
	}
	data, ok := plan.Data.(*planData)
	if !ok {
		return nil, fmt.Errorf("plan was not produced by the %s adapter", Name)
	}
	for _, f := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(f.Path, f.Contents, 0o644); err != nil {
			return nil, err
		}
	}

	k := newKubectl(env, data.opts)
	env.Logf("checking namespace %s", data.opts.Namespace)
	cluster, checks := preflight(ctx, k, data)
	result := func(ok bool, messages ...string) *adapter.Result {
		return &adapter.Result{OK: ok, Findings: checks, Messages: messages}
	}
	if adapter.HasErrors(checks) {
		return result(false, "Preflight checks failed; nothing was changed."), nil
	}
	if env.DryRun {
		return result(true, fmt.Sprintf("Dry run: preflight checks passed for namespace %s of %s; nothing was changed.", data.opts.Namespace, cluster)), nil
	}

	var versions []string
	for _, sc := range data.secrets {
		if err := ensureSecret(ctx, k, data, sc); err != nil {
			return result(false, err.Error()), nil
		}
		version, err := k.probe(ctx, "get", "secret", sc.object, "-o", "jsonpath={.metadata.resourceVersion}")
		if err != nil {
			return result(false, fmt.Sprintf("reading secret %s back failed: %v", sc.object, err)), nil
		}
		versions = append(versions, sc.name+"="+version)
	}
	data.versions = strings.Join(versions, ",")

	for i := range data.services {
		sv := &data.services[i]
		if sv.build == nil {
			continue
		}
		path := sv.build.dockerfile
		if sv.build.generated != nil {
			var err error
			if path, err = image.WriteGenerated(env.OutDir, sv.name, sv.build.generated); err != nil {
				return nil, err
			}
		}
		ref, err := image.BuildAndPush(ctx, env, image.Build{Context: sv.build.contextDir, Dockerfile: path,
			Repository: data.opts.imageRepository(sv.object), Platform: data.platform})
		if err != nil {
			return result(false, err.Error()), nil
		}
		sv.image = ref
	}

	// The file is written again with the digests, so what is reviewed
	// afterwards is what was sent.
	data.rollout = time.Now().UTC().Format(time.RFC3339)
	contents, err := manifests(data)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(env.OutDir, manifestsFile), contents, 0o644); err != nil {
		return nil, err
	}
	env.Logf("$ kubectl apply --prune -l %s=%s (%d objects)", projectLabel, data.project, 2*len(data.services))
	if err := k.run(ctx, bytes.NewReader(contents), nil, applyArgs(data.project)...); err != nil {
		return result(false, fmt.Sprintf("kubectl apply failed: %v", err)), nil
	}

	messages := []string{fmt.Sprintf("Deployed %s to namespace %s of %s.", data.project, data.opts.Namespace, cluster)}
	for _, sv := range data.services {
		env.Logf("$ kubectl rollout status deployment/%s", sv.object)
		if err := k.run(ctx, nil, nil, "rollout", "status", "deployment/"+sv.object, "--timeout", rolloutTimeout.String()); err != nil {
			return result(false, rolloutFailure(ctx, k, data, sv, err)...), nil
		}
		messages = append(messages, reachableAt(ctx, k, data, sv)...)
	}
	return result(true, messages...), nil
}

// reachableAt says where a service answers: inside the cluster, through its
// Ingress, and at its load balancer's address once the cluster assigns one.
func reachableAt(ctx context.Context, k kubectl, d *planData, sv service) []string {
	var lines []string
	if sv.http {
		lines = append(lines, fmt.Sprintf("%s: %s (inside the cluster)", sv.name, d.serviceURL(sv.name)))
	}
	if sv.ingress {
		hosts := []string{"any host"}
		if len(sv.svc.Domains) > 0 {
			hosts = nil
			for _, host := range sv.svc.Domains {
				hosts = append(hosts, "http://"+host)
			}
		}
		lines = append(lines, fmt.Sprintf("%s: %s through ingress class %s", sv.name, strings.Join(hosts, ", "), d.opts.IngressClass))
	}
	if sv.lb {
		addr, _ := k.probe(ctx, "get", "service", sv.object, "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}")
		if addr == "" {
			addr = "<address pending; anyship status shows it once assigned>"
		}
		for _, p := range sv.ports {
			if p.Protocol != spec.ProtocolHTTP && p.Exposure != spec.ExposureInternal {
				lines = append(lines, fmt.Sprintf("%s: %s:%d/%s (LoadBalancer)", sv.name, addr, p.Port, p.Protocol))
			}
		}
	}
	return lines
}

// applyArgs applies the manifests from stdin and removes the Deployments,
// Services and Ingresses of the spec that aren't in them any more: those are
// the kinds anyship makes from a service, and a Secret keeps its value until
// destroy --volumes.
func applyArgs(project string) []string {
	return []string{"apply", "-f", "-", "--prune", "-l", projectLabel + "=" + project,
		"--prune-allowlist", "apps/v1/Deployment", "--prune-allowlist", "core/v1/Service", "--prune-allowlist", "networking.k8s.io/v1/Ingress"}
}

// rolloutFailure explains a rollout that didn't finish: the service's pods
// and the cluster's recent warnings about them, which is where an image
// that can't be pulled or a container that keeps crashing shows up.
func rolloutFailure(ctx context.Context, k kubectl, d *planData, sv service, err error) []string {
	messages := []string{fmt.Sprintf("The rollout of %s didn't finish: %v. The previous pods, if any, keep serving.", sv.object, err)}
	pods, _ := k.probe(ctx, "get", "pods", "-l", projectLabel+"="+d.project+","+serviceLabel+"="+sv.name, "--no-headers")
	if pods != "" {
		messages = append(messages, "Pods:\n"+pods)
	}
	events, _ := k.probe(ctx, "get", "events", "--field-selector", "type=Warning", "--sort-by", ".lastTimestamp", "--no-headers")
	var about []string
	for _, line := range strings.Split(events, "\n") {
		if strings.Contains(line, sv.object) {
			about = append(about, line)
		}
	}
	if n := len(about); n > 0 {
		messages = append(messages, "Warnings:\n"+strings.Join(about[max(0, n-5):], "\n"))
	}
	return append(messages, fmt.Sprintf("Look closer with `kubectl %s describe deployment/%s` and `anyship logs -t kubernetes %s`.", strings.Join(k.args(), " "), sv.object, sv.name))
}

// preflight checks that kubectl is installed, that the context and the
// namespace exist, that this login may create Deployments there, and that
// images can be built. It returns the name of the cluster's context.
func preflight(ctx context.Context, k kubectl, d *planData) (string, []adapter.Finding) {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}

	if _, err := k.probe(ctx, "version", "--client"); err != nil {
		add(adapter.Error, "K8S_PREFLIGHT_KUBECTL", fmt.Sprintf("kubectl doesn't run on this machine: %v.", err), "Install it: https://kubernetes.io/docs/tasks/tools/")
		return "", findings
	}
	cluster := k.context
	if cluster == "" {
		current, err := k.probe(ctx, "config", "current-context")
		if err != nil || current == "" {
			add(adapter.Error, "K8S_PREFLIGHT_CONTEXT", "The kubeconfig has no current context.",
				"Set spec.targets.kubernetes.context to one of `kubectl config get-contexts`, or `kubectl config use-context <name>`.")
			return "", findings
		}
		cluster = current
	} else if _, err := k.probe(ctx, "config", "get-contexts", k.context, "-o", "name"); err != nil {
		add(adapter.Error, "K8S_PREFLIGHT_CONTEXT", fmt.Sprintf("Context %s isn't in the kubeconfig.", k.context),
			"Set spec.targets.kubernetes.context to one of `kubectl config get-contexts`, or leave it out to use the current one.")
		return "", findings
	}
	if _, err := k.probe(ctx, "get", "namespace", k.namespace, "-o", "name"); err != nil {
		if strings.Contains(err.Error(), "not found") {
			add(adapter.Error, "K8S_PREFLIGHT_NAMESPACE", fmt.Sprintf("Namespace %s doesn't exist in %s.", k.namespace, cluster),
				fmt.Sprintf("Create it (kubectl --context %s create namespace %s), or name an existing one in spec.targets.kubernetes.namespace.", cluster, k.namespace))
		} else {
			add(adapter.Error, "K8S_PREFLIGHT_CLUSTER", fmt.Sprintf("Can't reach the cluster of context %s: %v.", cluster, err),
				fmt.Sprintf("Check the login: kubectl --context %s cluster-info", cluster))
		}
		return cluster, findings
	}
	if allowed, err := k.probe(ctx, "auth", "can-i", "create", "deployments"); err != nil || allowed != "yes" {
		add(adapter.Error, "K8S_PREFLIGHT_ACCESS", fmt.Sprintf("This login can't create Deployments in namespace %s of %s.", k.namespace, cluster),
			fmt.Sprintf("Ask for the edit role there: kubectl --context %s create rolebinding anyship --clusterrole edit --user <you> -n %s", cluster, k.namespace))
		return cluster, findings
	}

	if slices.ContainsFunc(d.services, func(sv service) bool { return sv.ingress }) {
		if _, err := k.probe(ctx, "get", "ingressclass", d.opts.IngressClass, "-o", "name"); err != nil {
			classes, _ := k.probe(ctx, "get", "ingressclass", "-o", "name")
			have := "The cluster has none: install an ingress controller (ingress-nginx, Traefik) first."
			if classes != "" {
				have = "The cluster has " + strings.ReplaceAll(strings.ReplaceAll(classes, "ingressclass.networking.k8s.io/", ""), "\n", ", ") + "."
			}
			add(adapter.Error, "K8S_PREFLIGHT_INGRESS_CLASS", fmt.Sprintf("IngressClass %s doesn't exist in %s.", d.opts.IngressClass, cluster),
				have+" Set spec.targets.kubernetes.ingressClass to one of them.")
		}
	}
	if slices.ContainsFunc(d.services, func(sv service) bool { return sv.build != nil }) {
		if d.platform == "" {
			d.platform = nodePlatform(ctx, k, add)
		}
		if err := k.env.Exec(ctx, adapter.ExecOptions{Dir: k.env.Dir, Stdout: io.Discard, Stderr: io.Discard}, "docker", "buildx", "version"); err != nil {
			add(adapter.Error, "K8S_PREFLIGHT_DOCKER", "Building from source needs Docker with buildx on this machine.", "Install Docker Desktop or the buildx plugin, or set services.<name>.image.")
		}
	}
	if !adapter.HasErrors(findings) {
		message := fmt.Sprintf("Namespace %s of %s is ready.", k.namespace, cluster)
		if d.platform != "" {
			message += " Images are built for " + d.platform + "."
		}
		add(adapter.Info, "K8S_PREFLIGHT_OK", message, "")
	}
	return cluster, findings
}

// nodePlatform is the platform to build images for: that of the nodes when
// they all share one, otherwise the default, which the user can override
// with the platform option.
func nodePlatform(ctx context.Context, k kubectl, add func(level adapter.Level, code, message, hint string)) string {
	out, err := k.probe(ctx, "get", "nodes", "-o", "jsonpath={.items[*].status.nodeInfo.architecture}")
	archs := slices.Compact(slices.Sorted(slices.Values(strings.Fields(out))))
	switch {
	case err != nil || len(archs) == 0:
		add(adapter.Info, "K8S_PREFLIGHT_PLATFORM", fmt.Sprintf("Couldn't read the nodes' architecture, so images are built for %s.", image.DefaultPlatform),
			"Set spec.targets.kubernetes.platform if the nodes run something else.")
		return image.DefaultPlatform
	case len(archs) > 1:
		add(adapter.Info, "K8S_PREFLIGHT_PLATFORM", fmt.Sprintf("The nodes run %s, so images are built for %s.", strings.Join(archs, " and "), image.DefaultPlatform),
			"Set spec.targets.kubernetes.platform to build for the others.")
		return image.DefaultPlatform
	}
	return "linux/" + archs[0]
}

// ensureSecret makes the Secret exist with the right value: a value from the
// deployer's environment always wins, a missing generated secret gets a random
// one, and a missing secret with neither is an error.
func ensureSecret(ctx context.Context, k kubectl, d *planData, sc secret) error {
	value, fromEnv := k.env.LookupEnv(sc.name)
	fromEnv = fromEnv && value != ""
	_, err := k.probe(ctx, "get", "secret", sc.object, "-o", "name")
	exists := err == nil

	switch {
	case exists && !fromEnv:
		return nil
	case !fromEnv && !sc.generate:
		return fmt.Errorf("secret %s has no value: export %s and apply again", sc.object, sc.name)
	case !fromEnv:
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		value = hex.EncodeToString(buf)
		k.env.Logf("$ kubectl apply secret/%s (generated)", sc.object)
	default:
		k.env.Logf("$ kubectl apply secret/%s (value from $%s)", sc.object, sc.name)
	}
	contents, err := render(d.secret(sc, value), "")
	if err != nil {
		return err
	}
	if err := k.run(ctx, bytes.NewReader(contents), io.Discard, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("writing secret %s failed: %w", sc.object, err)
	}
	return nil
}
