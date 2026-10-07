package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
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
	} `json:"spec"`
	Status struct {
		Available  int `json:"availableReplicas"`
		Updated    int `json:"updatedReplicas"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// pod is what status reads from `kubectl get pods -o json`: the state of a
// container that isn't running, which names what is wrong (ImagePullBackOff,
// CrashLoopBackOff) long before the Deployment gives up.
type pod struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Containers []struct {
			Ready bool `json:"ready"`
			State struct {
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

func getJSON[T any](ctx context.Context, k kubectl, kind, selector string) ([]T, error) {
	out, err := k.probe(ctx, "get", kind, "-l", selector, "-o", "json")
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
		if so, ok := balancers[objectName(s.Name, name)]; ok {
			if addr := so.address(); addr != "" {
				ss.Detail += ", load balancer " + addr
			} else {
				ss.Detail += ", load balancer address pending"
			}
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
		for _, p := range pods {
			if p.Metadata.Labels[serviceLabel] != name {
				continue
			}
			for _, c := range p.Status.Containers {
				switch {
				case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "ContainerCreating":
					ss.State, ss.Health, ss.Detail = c.State.Waiting.Reason, "unhealthy", strings.TrimSpace(c.State.Waiting.Message)
				case c.State.Terminated != nil && c.State.Terminated.Reason != "" && c.State.Terminated.Reason != "Completed":
					ss.State, ss.Health = c.State.Terminated.Reason, "unhealthy"
				}
			}
		}
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
	lines := []string{fmt.Sprintf("Delete the Deployments, Services and Ingresses %s in %s.", strings.Join(names, ", "), where)}
	secrets := secretNames(s)
	switch {
	case opts.Volumes && len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the Secrets %s. Their values cannot be recovered.", strings.Join(secrets, ", ")))
	case len(secrets) > 0:
		lines = append(lines, fmt.Sprintf("Keep the Secrets %s; destroy --volumes deletes them.", strings.Join(secrets, ", ")))
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
	found, err := k.probe(ctx, "get", "deployments,services,ingresses", "-l", selector, "-o", "name")
	if err != nil {
		return nil, fmt.Errorf("listing the objects of %s in namespace %s failed (%w)", s.Name, o.Namespace, err)
	}
	kinds := "deployments,services,ingresses"
	if opts.Volumes {
		kinds += ",secrets"
	}
	if found == "" && !opts.Volumes {
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Nothing to remove: %s has no Deployments, Services or Ingresses in namespace %s.", s.Name, o.Namespace)}}, nil
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
