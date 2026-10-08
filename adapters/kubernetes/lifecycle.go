package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

const defaultLogLines = 100

// deployment is what status reads from `kubectl get deployments -o json`.
type deployment struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas int `json:"replicas"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Available  int `json:"availableReplicas"`
		Updated    int `json:"updatedReplicas"`
		Conditions []struct {
			Type           string `json:"type"`
			Status         string `json:"status"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			LastUpdateTime string `json:"lastUpdateTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// since is when the Deployment's pods last changed: the time apply stamped
// on the pod template, or else when the rollout last progressed.
func (d deployment) since() string {
	if t := d.Spec.Template.Metadata.Annotations[rolloutAnnotation]; t != "" {
		return t
	}
	for _, c := range d.Status.Conditions {
		if c.Type == "Progressing" && c.LastUpdateTime != "" {
			return c.LastUpdateTime
		}
	}
	return ""
}

// pod is what status reads from `kubectl get pods -o json`: the state of a
// container that isn't running, which names what is wrong (ImagePullBackOff,
// CrashLoopBackOff) long before the Deployment gives up.
type pod struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Containers []struct {
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			Image        string `json:"image"`
			ImageID      string `json:"imageID"`
			State        struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// serviceObject is what status reads from `kubectl get services -o json`:
// the address a LoadBalancer was given.
type serviceObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Type string `json:"type"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

func (so serviceObject) address() string {
	for _, in := range so.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
		if in.Hostname != "" {
			return in.Hostname
		}
	}
	return ""
}

// ingressObject is what status reads from `kubectl get ingresses -o json`:
// the address the ingress controller gave an Ingress, and its hosts.
type ingressObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Rules []struct {
			Host string `json:"host"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

// url is where the Ingress answers: its first host, or else its address;
// "pending" until the controller assigns one.
func (in ingressObject) url() string {
	addr := ""
	for _, lb := range in.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			addr = lb.IP
		} else if lb.Hostname != "" {
			addr = lb.Hostname
		}
		if addr != "" {
			break
		}
	}
	for _, r := range in.Spec.Rules {
		if r.Host != "" {
			if addr == "" {
				return "pending"
			}
			return "http://" + r.Host
		}
	}
	if addr == "" {
		return "pending"
	}
	return "http://" + addr
}

// event is what status reads from `kubectl get events -o json`: a warning
// about one object.
type event struct {
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	LastTimestamp string `json:"lastTimestamp"`
	Count         int    `json:"count"`
}

// maxEvents bounds the warnings status reports per service.
const maxEvents = 5

// warningEvents lists the namespace's warning events; none when the cluster
// refuses or the output is not what kubectl prints.
func warningEvents(ctx context.Context, k kubectl) []event {
	out, err := k.probe(ctx, "get", "events", "--field-selector", "type=Warning", "-o", "json")
	if err != nil {
		return nil
	}
	var l struct {
		Items []event `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil
	}
	return l.Items
}

// warningsAbout picks the latest warnings about a service's objects: its
// pods, and the Deployment itself.
func warningsAbout(events []event, pods map[string]bool, object string) []string {
	var about []event
	for _, e := range events {
		if pods[e.InvolvedObject.Name] || e.InvolvedObject.Name == object {
			about = append(about, e)
		}
	}
	slices.SortStableFunc(about, func(a, b event) int { return strings.Compare(b.LastTimestamp, a.LastTimestamp) })
	var out []string
	for _, e := range about[:min(len(about), maxEvents)] {
		line := e.Reason + ": " + strings.TrimSpace(e.Message)
		if e.Count > 1 {
			line += fmt.Sprintf(" (x%d)", e.Count)
		}
		out = append(out, line)
	}
	return out
}

// imageOf is the image a pod runs, by digest when the kubelet reports one.
func imageOf(p pod) string {
	for _, c := range p.Status.Containers {
		if id := strings.TrimPrefix(c.ImageID, "docker-pullable://"); id != "" {
			return id
		}
		if c.Image != "" {
			return c.Image
		}
	}
	return ""
}

func getJSON[T any](ctx context.Context, k kubectl, kind, selector string) ([]T, error) {
	args := []string{"get", kind, "-o", "json"}
	if selector != "" {
		args = []string{"get", kind, "-l", selector, "-o", "json"}
	}
	out, err := k.probe(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("listing %s in namespace %s failed (%w)", kind, k.namespace, err)
	}
	var l struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil, fmt.Errorf("unexpected output from kubectl get %s: %w", kind, err)
	}
	return l.Items, nil
}

func (a *Adapter) Status(ctx context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Status, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.kubernetes: %w", err)
	}
	k := newKubectl(env, *o)
	selector := projectLabel + "=" + s.Name
	deployments, err := getJSON[deployment](ctx, k, "deployments", selector)
	if err != nil {
		return nil, err
	}
	pods, err := getJSON[pod](ctx, k, "pods", selector)
	if err != nil {
		return nil, err
	}
	services, err := getJSON[serviceObject](ctx, k, "services", selector)
	if err != nil {
		return nil, err
	}
	ingresses, err := getJSON[ingressObject](ctx, k, "ingresses", selector)
	if err != nil {
		return nil, err
	}
	// Events carry no labels; they are matched to the pods below. A cluster
	// that refuses to list them costs the warnings, not the status.
	events := warningEvents(ctx, k)
	found := map[string]deployment{}
	for _, d := range deployments {
		found[d.Metadata.Name] = d
	}
	balancers := map[string]serviceObject{}
	for _, so := range services {
		if so.Spec.Type == "LoadBalancer" {
			balancers[so.Metadata.Name] = so
		}
	}
	ingressed := map[string]ingressObject{}
	for _, in := range ingresses {
		ingressed[in.Metadata.Name] = in
	}

	cluster := o.Context
	if cluster == "" {
		cluster, _ = k.probe(ctx, "config", "current-context")
	}
	st := &adapter.Status{Target: Name, Location: cluster + "/" + o.Namespace, Deployed: len(found) > 0}
	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		ss := adapter.ServiceStatus{Name: name, State: "missing", Desired: replicas(svc)}
		d, ok := found[objectName(s.Name, name)]
		if !ok {
			st.Services = append(st.Services, ss)
			continue
		}
		ss.Desired, ss.Running = d.Spec.Replicas, d.Status.Available
		ss.Detail = fmt.Sprintf("%d/%d available", d.Status.Available, d.Spec.Replicas)
		ss.Since = d.since()
		if so, ok := balancers[objectName(s.Name, name)]; ok {
			ss.URL = "pending"
			if addr := so.address(); addr != "" {
				ss.URL = addr
				if len(svc.Ports) > 0 {
					ss.URL += ":" + strconv.Itoa(svc.Ports[0].Port)
				}
			}
		}
		if in, ok := ingressed[objectName(s.Name, name)]; ok {
			ss.URL = in.url()
		}
		for _, p := range svc.Ports {
			ss.Ports = append(ss.Ports, strconv.Itoa(p.Port)+"/"+string(p.Protocol))
		}
		ss.State = "deploying"
		if d.Status.Available >= d.Spec.Replicas && d.Status.Updated >= d.Spec.Replicas {
			ss.State = "running"
		}
		for _, c := range d.Status.Conditions {
			if c.Type == "Progressing" && c.Status == "False" {
				ss.State, ss.Health, ss.Detail = "failing", "unhealthy", c.Message
			}
		}
		// A pod that can't start says why; the Deployment only says it
		// is still progressing.
		restarts, mine := 0, map[string]bool{}
		for _, p := range pods {
			if p.Metadata.Labels[serviceLabel] != name {
				continue
			}
			mine[p.Metadata.Name] = true
			if ss.Image == "" {
				ss.Image = imageOf(p)
			}
			for _, c := range p.Status.Containers {
				restarts += c.RestartCount
				switch {
				case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "ContainerCreating":
					ss.State, ss.Health, ss.Detail = c.State.Waiting.Reason, "unhealthy", strings.TrimSpace(c.State.Waiting.Message)
				case c.State.Terminated != nil && c.State.Terminated.Reason != "" && c.State.Terminated.Reason != "Completed":
					ss.State, ss.Health = c.State.Terminated.Reason, "unhealthy"
				}
			}
		}
		ss.Restarts = &restarts
		ss.Events = warningsAbout(events, mine, objectName(s.Name, name))
		if ss.Health == "" && svc.HealthCheck != nil && (svc.HealthCheck.Path != "" || svc.HealthCheck.Command != "") {
			ss.Health = "starting"
			if ss.State == "running" {
				ss.Health = "healthy"
			}
		}
		st.Services = append(st.Services, ss)
	}
	return st, nil
}

// Logs reads the pods' logs with `kubectl logs`, by label, so pods that
// replaced one another are all included.
func (a *Adapter) Logs(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return fmt.Errorf("spec.targets.kubernetes: %w", err)
	}
	selector := projectLabel + "=" + s.Name
	if opts.Service != "" {
		selector += "," + serviceLabel + "=" + opts.Service
	}
	lines := opts.Tail
	if lines == 0 {
		lines = defaultLogLines
	}
	args := []string{"logs", "-l", selector, "--all-containers", "--prefix", "--max-log-requests", "50", "--tail", strconv.Itoa(lines)}
	if opts.Since != "" {
		if _, err := time.ParseDuration(opts.Since); err == nil {
			args = append(args, "--since", opts.Since)
		} else {
			args = append(args, "--since-time", opts.Since)
		}
	}
	if opts.Timestamps {
		args = append(args, "--timestamps")
	}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if err := newKubectl(env, *o).run(ctx, nil, nil, args...); err != nil {
		return fmt.Errorf("reading logs failed (%w); has the spec been deployed with `anyship apply -t kubernetes`?", err)
	}
	return nil
}

func (a *Adapter) DestroySummary(s *spec.Spec, opts adapter.DestroyOptions) ([]string, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.kubernetes: %w", err)
	}
	var names []string
	for _, name := range s.ServiceNames() {
		names = append(names, objectName(s.Name, name))
	}
	where := "namespace " + o.Namespace
	if o.Context != "" {
		where += " of " + o.Context
	}
	lines := []string{fmt.Sprintf("Delete the Deployments, Services, Ingresses, CronJobs and autoscalers of %s in %s.", strings.Join(names, ", "), where)}
	secrets := secretNames(s)
	switch {
	case opts.Volumes && len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the Secrets %s. Their values cannot be recovered.", strings.Join(secrets, ", ")))
	case len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("Keep the Secrets %s; destroy --volumes deletes them.", strings.Join(secrets, ", ")))
	}
	var claims []string
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			claims = append(claims, objectName(s.Name, name)+"-"+v.Name)
		}
	}
	switch {
	case opts.Volumes && len(claims) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the volumes %s (PersistentVolumeClaims). Their data cannot be recovered.", strings.Join(claims, ", ")))
	case len(claims) > 0:
		lines = append(lines, fmt.Sprintf("Keep the volumes %s; destroy --volumes deletes them.", strings.Join(claims, ", ")))
	}
	return append(lines, "Keep the namespace and the images in the registry."), nil
}

func (a *Adapter) Destroy(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.kubernetes: %w", err)
	}
	k := newKubectl(env, *o)
	selector := projectLabel + "=" + s.Name
	found, err := k.probe(ctx, "get", "deployments,services,ingresses,cronjobs,horizontalpodautoscalers", "-l", selector, "-o", "name")
	if err != nil {
		return nil, fmt.Errorf("listing the objects of %s in namespace %s failed (%w)", s.Name, o.Namespace, err)
	}
	kinds := "deployments,services,ingresses,cronjobs,horizontalpodautoscalers"
	if opts.Volumes {
		kinds += ",secrets,persistentvolumeclaims"
	}
	if found == "" && !opts.Volumes {
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Nothing to remove: %s has no Deployments, Services, Ingresses, CronJobs or autoscalers in namespace %s.", s.Name, o.Namespace)}}, nil
	}
	env.Logf("$ kubectl delete %s -l %s", kinds, selector)
	if err := k.run(ctx, nil, nil, "delete", kinds, "-l", selector, "--ignore-not-found"); err != nil {
		return &adapter.Result{Messages: []string{fmt.Sprintf("deleting the objects of %s failed: %v", s.Name, err)}}, nil
	}
	return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Removed %s from namespace %s.", s.Name, o.Namespace)}}, nil
}

func secretNames(s *spec.Spec) []string {
	var names []string
	for _, name := range sortedKeys(s.Secrets) {
		names = append(names, secretName(s.Name, name))
	}
	return names
}
