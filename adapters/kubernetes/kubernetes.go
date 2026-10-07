// Package kubernetes deploys a spec to a Kubernetes cluster through kubectl.
//
// Each spec service becomes a Deployment and a ClusterIP Service named
// <spec>-<service> in the namespace the spec names, labeled
// anyship-project=<spec>. That label is how status, logs and destroy find
// them, and how apply prunes what the spec no longer names, since anyship
// keeps no state. Services built from source are built locally and pushed to
// a registry you provide, then deployed by digest. Secrets are Secret
// objects, mounted as files under /run/secrets.
package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/image"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/spec"
)

// Name is the target name used in `--target` and `targets.kubernetes`.
const Name = "kubernetes"

const (
	projectLabel = "anyship-project"
	serviceLabel = "anyship-service"
	// cronLabel marks the pods of a service's cron jobs instead of
	// serviceLabel, so the Service doesn't send them traffic and status
	// doesn't count them as the service.
	cronLabel   = "anyship-cron"
	defaultPort = 8080
	// secretsDir is where a service's secrets are mounted, one file each.
	secretsDir = "/run/secrets"
	// manifestsFile is the generated file with every object apply sends.
	manifestsFile = "manifests.yaml"
)

// rolloutAnnotation is set on the pod template of a service whose image tag
// can move, with the time of the apply, so that every apply rolls the pods
// and pulls the tag again.
const rolloutAnnotation = "anyship.dev/rollout"

// secretsAnnotation is set on the pod template of a service with secrets,
// with the resource versions of its Secrets, so that a changed value rolls
// the pods: a file mounted with subPath doesn't follow the Secret.
const secretsAnnotation = "anyship.dev/secrets"

// Options is the targets.kubernetes block of a spec.
type Options struct {
	// Context is the kubeconfig context to deploy through (`kubectl config
	// get-contexts`); the current one when empty.
	Context string `json:"context,omitempty"`
	// Namespace is the existing namespace the spec is deployed to.
	Namespace string `json:"namespace"`
	// Repository is where images built from source are pushed: a registry
	// path the cluster's nodes can pull from, such as ghcr.io/acme/shop or
	// localhost:5000/shop. Each service's image goes to
	// <repository>/<spec>-<service>.
	Repository string `json:"repository,omitempty"`
	// Platform is what images are built for, such as linux/arm64; the
	// architecture of the cluster's nodes when empty.
	Platform string `json:"platform,omitempty"`
	// IngressClass is the IngressClass (`kubectl get ingressclass`) an
	// Ingress is made with for every public HTTP port; without it no
	// Ingress is made and a public port is reachable inside the cluster only.
	IngressClass string `json:"ingressClass,omitempty"`
	// CronImage is the image a cron entry with a path runs curl from;
	// curlimages/curl when empty. Name another one on a cluster that can
	// only pull from its own registry.
	CronImage string `json:"cronImage,omitempty"`
}

// defaultCronImage calls the path of a cron entry; pinned, so a deploy
// doesn't change under a moving tag.
const defaultCronImage = "curlimages/curl:8.14.1"

func (o Options) cronImage() string {
	if o.CronImage != "" {
		return o.CronImage
	}
	return defaultCronImage
}

// The probes made from healthCheck: the startup probe asks every probePeriod
// seconds and gives up after probeFailures tries, 4 minutes, after which the
// rollout fails; the readiness probe then keeps asking and takes a pod out of
// the Service after readinessFailures misses.
const (
	probePeriod       = 10
	probeTimeout      = 5
	probeFailures     = 24
	readinessFailures = 3
)

var (
	dnsLabelRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	repositoryRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)+$`)
	platformRe   = regexp.MustCompile(`^linux/[a-z0-9]+(/v[0-9]+)?$`)
	// Object names other than labels, such as an IngressClass's, are DNS
	// subdomains.
	dnsSubdomainRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

type Adapter struct{}

var (
	_ adapter.Adapter      = (*Adapter)(nil)
	_ adapter.LogReader    = (*Adapter)(nil)
	_ adapter.StatusReader = (*Adapter)(nil)
	_ adapter.Destroyer    = (*Adapter)(nil)
)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return Name }
func (*Adapter) Description() string {
	return "Kubernetes (Deployments, Services, Secrets in a namespace) via kubectl"
}

// service is one spec service to deploy, as a Deployment and a Service.
type service struct {
	name   string // spec service name
	object string // name of the Deployment and the Service
	svc    *spec.Service
	// port is the HTTP port, if the service has one (http is true then),
	// and internal whether it is reachable inside the cluster only.
	port     int
	http     bool
	internal bool
	// ports are all the service's ports, the HTTP one first.
	ports []spec.Port
	// ingress is whether an Ingress routes to the HTTP port, and lb whether
	// the Service is a LoadBalancer, for a public TCP or UDP port.
	ingress bool
	lb      bool
	argv    []string
	// jobs are the service's cron entries, as CronJobs.
	jobs []cronJob
	// image is the image to deploy; for builds it is filled in by Apply.
	image string
	// moving is whether the image's tag can move, in which case every
	// apply rolls the pods so the tag is pulled again.
	moving bool
	build  *build
}

// cronJob is one cron entry of a service: a path to call, or a command to
// run in the service's image.
type cronJob struct {
	id       string // CronJob name
	schedule string
	method   string
	path     string
	argv     []string
}

func jobName(project, service string, n int) string {
	return fmt.Sprintf("%s-%s-cron-%d", project, service, n)
}

type build struct {
	contextDir string
	// dockerfile is the user's Dockerfile, or empty when generated is set.
	dockerfile string
	generated  *dockerfile.Result
}

type secret struct {
	name     string // spec secret name, the file under /run/secrets
	object   string // Secret object name
	generate bool
}

// planData is handed from Plan to Apply.
type planData struct {
	opts     Options
	project  string
	services []service
	secrets  []secret
	// platform is what images are built for; preflight fills it in from
	// the nodes when the options don't say.
	platform string
	// rollout stamps the pod templates of services with a moving image
	// tag; Apply sets it to the time of the apply.
	rollout string
	// versions stamps the pod templates of services with secrets; Apply
	// sets it to the Secrets' resource versions.
	versions string
}

func objectName(project, service string) string { return project + "-" + service }

// secretName is the Secret object for a spec secret: shop's API_KEY becomes
// shop-api-key, since object names are DNS labels.
func secretName(project, name string) string {
	return project + "-" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

func (o Options) imageRepository(object string) string { return o.Repository + "/" + object }

// serviceURL is the address a service of the spec has inside the cluster,
// which follows from the names, so it is known before the first deploy.
func (d *planData) serviceURL(name string) string {
	port := defaultPort
	for _, sv := range d.services {
		if sv.name == name {
			port = sv.port
		}
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", objectName(d.project, name), d.opts.Namespace, port)
}

// httpPort is the port other services and an Ingress reach a service on:
// its first HTTP port, or the default when it names no port at all. A
// service with ports but no HTTP one has none.
func httpPort(svc *spec.Service) (int, bool) {
	for _, p := range svc.Ports {
		if p.Protocol == spec.ProtocolHTTP {
			return p.Port, true
		}
	}
	if len(svc.Ports) == 0 {
		return defaultPort, true
	}
	return 0, false
}

func (a *Adapter) Plan(_ context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	plan := &adapter.Plan{Target: Name}
	opts, err := decodeOptions(s.Targets[Name])
	if err != nil {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level: adapter.Error, Code: "K8S_BAD_OPTIONS", Message: "spec.targets.kubernetes: " + err.Error(),
			Hint: adapter.OptionsHint(Name, "`namespace: my-app`, plus `repository: ghcr.io/me/my-app` to build from source"),
		})
		return plan, nil
	}

	data := &planData{opts: *opts, project: s.Name, platform: opts.Platform, rollout: "<time of the apply>", versions: "<resource versions of the secrets>"}
	used := map[string]bool{}
	dependsNoted := false
	for _, name := range s.DeployOrder() {
		svc := s.Services[name]
		sv, findings := checkService(name, svc, s, opts, env.Dir)
		plan.Findings = append(plan.Findings, findings...)
		if len(svc.DependsOn) > 0 && !dependsNoted {
			dependsNoted = true
			plan.Findings = append(plan.Findings, adapter.Finding{
				Level: adapter.Info, Code: "K8S_DEPENDS_ON", Service: name,
				Message: "Every object is applied at once; dependsOn only orders the wait for each rollout. Services reach each other by name.",
			})
		}
		for _, secretName := range svc.Secrets {
			used[secretName] = true
		}
		data.services = append(data.services, sv)
	}
	if adapter.HasErrors(plan.Findings) {
		return plan, nil
	}

	for _, name := range sortedKeys(used) {
		sc := secret{name: name, object: secretName(s.Name, name), generate: s.Secrets[name].Generate != ""}
		data.secrets = append(data.secrets, sc)
		detail := "read from $" + name + " when set; otherwise the existing value in the cluster is kept"
		if sc.generate {
			detail = "generated if it doesn't exist yet, then kept (set $" + name + " to override)"
		}
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpNote, Kind: "Secret", Name: sc.object, Detail: detail})
	}

	for _, sv := range data.services {
		if sv.build != nil {
			plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpRun, Kind: "image", Name: opts.imageRepository(sv.object), Detail: "docker buildx build --push, deployed by digest"})
			if sv.build.generated != nil {
				plan.Files = append(plan.Files, adapter.File{Path: filepath.Join(env.OutDir, sv.name+".Dockerfile"), Contents: sv.build.generated.Dockerfile})
			}
		}
		kind := "Deployment and Service"
		if sv.ingress {
			kind = "Deployment, Service and Ingress"
		}
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpDeploy, Kind: kind, Name: sv.object,
			Detail: fmt.Sprintf("in namespace %s, %d replica(s), %s", opts.Namespace, replicas(sv.svc), sv.imageOrPlaceholder(*opts))})
		for _, job := range sv.jobs {
			detail := fmt.Sprintf("%q runs %s in the service's image", job.schedule, strings.Join(job.argv, " "))
			if job.path != "" {
				detail = fmt.Sprintf("%q calls %s %s on %s", job.schedule, job.method, job.path, sv.object)
			}
			plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpDeploy, Kind: "CronJob", Name: job.id, Detail: detail})
		}
	}
	contents, err := manifests(data)
	if err != nil {
		return nil, err
	}
	plan.Files = append(plan.Files, adapter.File{Path: filepath.Join(env.OutDir, manifestsFile), Contents: contents})
	plan.Data = data
	return plan, nil
}

func (sv service) imageOrPlaceholder(o Options) string {
	if sv.image != "" {
		return sv.image
	}
	return o.imageRepository(sv.object) + "@<digest from the build>"
}

func replicas(svc *spec.Service) int {
	if svc.Replicas > 1 {
		return svc.Replicas
	}
	return 1
}

func checkService(name string, svc *spec.Service, s *spec.Spec, opts *Options, dir string) (service, []adapter.Finding) {
	sv := service{name: name, object: objectName(s.Name, name), svc: svc, port: defaultPort}
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Service: name, Message: message, Hint: hint})
	}

	if !dnsLabelRe.MatchString(sv.object) {
		add(adapter.Error, "K8S_NAME", fmt.Sprintf("The object name %q is invalid or longer than 63 characters.", sv.object), "Shorten the spec or service name.")
	}
	switch svc.Kind {
	case spec.KindStatic:
		add(adapter.Error, "K8S_STATIC", "Static sites aren't deployed to Kubernetes by anyship yet.", "Deploy the site to the cloudflare or vps target for now.")
	case spec.KindWorker:
		add(adapter.Error, "K8S_WORKER", "Background workers aren't supported on the kubernetes target yet.", "Run the worker on the vps target for now.")
	}
	if len(svc.Volumes) > 0 {
		add(adapter.Error, "K8S_VOLUMES", "Volumes aren't supported on the kubernetes target yet.", "Store data in an external database, or use the vps target for now.")
	}
	var http []spec.Port
	for _, p := range svc.Ports {
		if p.Protocol == spec.ProtocolHTTP {
			http = append(http, p)
			continue
		}
		sv.ports = append(sv.ports, p)
		if p.Exposure != spec.ExposureInternal {
			sv.lb = true
		}
	}
	switch {
	case len(http) > 1:
		add(adapter.Error, "K8S_MULTIPLE_PORTS", "The kubernetes target sends HTTP traffic to one port per service.", "Keep one HTTP port, or split the service.")
	case len(http) == 1:
		sv.port, sv.http, sv.internal = http[0].Port, true, http[0].Exposure == spec.ExposureInternal
		sv.ports = append([]spec.Port{http[0]}, sv.ports...)
		switch {
		case sv.internal:
		case opts.IngressClass != "":
			sv.ingress = true
			hosts := "any host"
			if len(svc.Domains) > 0 {
				hosts = strings.Join(svc.Domains, ", ")
			}
			add(adapter.Info, "K8S_INGRESS",
				fmt.Sprintf("Port %d is public: an Ingress of class %s routes %s to %s. anyship issues no certificate and makes no DNS record; TLS is the ingress controller's to add.", sv.port, opts.IngressClass, hosts, sv.object), "")
		case len(svc.Domains) > 0:
			add(adapter.Error, "K8S_DOMAINS_NEED_INGRESS",
				fmt.Sprintf("domains (%s) are routed by an Ingress, and the spec names no IngressClass to make one with.", strings.Join(svc.Domains, ", ")),
				"Set spec.targets.kubernetes.ingressClass to one of `kubectl get ingressclass`, or install an ingress controller first.")
		default:
			add(adapter.Warning, "K8S_PUBLIC_PORT",
				fmt.Sprintf("Port %d is public, but without spec.targets.kubernetes.ingressClass no Ingress is made: %s is reachable inside the cluster only, at http://%s.%s.svc:%d.", sv.port, sv.object, sv.object, opts.Namespace, sv.port),
				fmt.Sprintf("Set ingressClass to one of `kubectl get ingressclass` to route it. Until then `kubectl port-forward -n %s svc/%s %d:%d` reaches it from this machine.", opts.Namespace, sv.object, sv.port, sv.port))
		}
	case len(sv.ports) == 0 && svc.Kind == spec.KindServer:
		add(adapter.Info, "K8S_PORT_ASSUMED", fmt.Sprintf("No port in the spec; the Service will send traffic to %d (also passed as $PORT).", defaultPort), "")
		sv.http = true
		sv.ports = []spec.Port{{Port: defaultPort, Protocol: spec.ProtocolHTTP}}
	}
	if sv.lb {
		var public []string
		for _, p := range sv.ports {
			if p.Protocol != spec.ProtocolHTTP && p.Exposure != spec.ExposureInternal {
				public = append(public, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
			}
		}
		add(adapter.Info, "K8S_LOAD_BALANCER",
			fmt.Sprintf("Port %s is public, so %s is a Service of type LoadBalancer: the cluster's load balancer gives it an address, which `anyship status` shows once assigned.", strings.Join(public, ", "), sv.object),
			"A cluster without a load balancer implementation (a bare kind or minikube) leaves the address pending for good.")
	}
	for i, c := range svc.Cron {
		job := cronJob{id: jobName(s.Name, name, i), schedule: c.Schedule, method: c.Method, path: c.Path}
		switch {
		case !dnsLabelRe.MatchString(job.id):
			add(adapter.Error, "K8S_NAME", fmt.Sprintf("The CronJob name %q is invalid or longer than 63 characters.", job.id), "Shorten the spec or service name.")
			continue
		case c.Path != "" && !sv.http:
			add(adapter.Error, "K8S_CRON_PATH", fmt.Sprintf("cron %q calls %s, but the service has no HTTP port to call it on.", c.Schedule, c.Path), "Give the service an HTTP port, or run a command instead.")
			continue
		case c.Path != "":
		case c.Command != "":
			argv, err := shellwords.Split(c.Command)
			if err != nil {
				add(adapter.Error, "K8S_BAD_CRON", fmt.Sprintf("cron %q command %s.", c.Schedule, err), "")
				continue
			}
			job.argv = argv
		default:
			add(adapter.Error, "K8S_CRON", fmt.Sprintf("cron %q has neither a command nor a path.", c.Schedule), "Give the entry a command to run in the service's image, or a path of the service to call.")
			continue
		}
		sv.jobs = append(sv.jobs, job)
	}
	if len(sv.jobs) > 0 {
		add(adapter.Info, "K8S_CRON",
			fmt.Sprintf("cron entries become CronJobs named %s-cron-<n>, read in UTC. A path is called inside the cluster at http://%s.%s.svc:%d<path> by %s; a command runs in the service's image with its env and secrets. Jobs don't overlap: a run still going when the next is due skips it.", sv.object, sv.object, opts.Namespace, sv.port, opts.cronImage()), "")
	}
	for _, ref := range svc.RefersTo() {
		if other, ok := s.Services[ref]; ok {
			if _, ok := httpPort(other); !ok {
				add(adapter.Error, "K8S_SERVICE_URL", fmt.Sprintf("env refers to ${services.%s.url}, but %s has no HTTP port to reach it on.", ref, ref), "Give that service an HTTP port, or drop the reference.")
			}
		}
	}

	for _, resourceName := range svc.Uses {
		if s.Resources[resourceName].External {
			add(adapter.Info, "K8S_EXTERNAL_RESOURCE", fmt.Sprintf("Resource %q is external; pass its connection settings through env or secrets.", resourceName), "")
		} else {
			add(adapter.Error, "K8S_RESOURCE", fmt.Sprintf("anyship doesn't provision %s on Kubernetes (resource %q).", s.Resources[resourceName].Type, resourceName),
				"Run it yourself (an operator, a managed database) and mark the resource external.")
		}
	}

	if svc.Start != "" {
		argv, err := shellwords.Split(svc.Start)
		if err != nil {
			add(adapter.Error, "K8S_BAD_START", "start "+err.Error()+".", "")
		}
		sv.argv = argv
	} else if svc.Entry != "" {
		add(adapter.Info, "K8S_ENTRY_IGNORED", "entry is only used by edge runtimes; the image's own command runs on Kubernetes.", "")
	}
	if hc := svc.HealthCheck; hc != nil {
		switch {
		case hc.Path != "" && !strings.HasPrefix(hc.Path, "/"):
			add(adapter.Error, "K8S_BAD_HEALTH_PATH", fmt.Sprintf("healthCheck.path %q must start with a slash.", hc.Path), "")
		case hc.Path != "":
			add(adapter.Info, "K8S_HEALTH_CHECK",
				fmt.Sprintf("healthCheck.path is the startup and readiness probe: a new pod gets traffic once GET %s answers with a 2xx or 3xx status, and the rollout fails if it hasn't within %d minutes.", hc.Path, probePeriod*probeFailures/60), "")
		case hc.Command != "":
			add(adapter.Info, "K8S_HEALTH_CHECK",
				fmt.Sprintf("healthCheck.command is the startup and readiness probe, run in the container with sh -c: a new pod gets traffic once it exits 0, and the rollout fails if it hasn't within %d minutes.", probePeriod*probeFailures/60), "")
		}
	}

	if svc.Image != "" {
		sv.image = svc.Image
		if tag, moving := image.MovingTag(svc.Image); moving {
			sv.moving = true
			add(adapter.Info, "K8S_MUTABLE_TAG",
				fmt.Sprintf("image %s is deployed by the tag %q, which can move, so every apply rolls the pods and pulls it again; they run what the tag points at then.", svc.Image, tag),
				fmt.Sprintf("Pin it by digest in anyship.yaml (image: name@sha256:…) for a rollout only when the spec changes, or override it for one deploy: `anyship apply --image %s=<ref>`.", name))
		}
		return sv, findings
	}
	if opts.Repository == "" {
		add(adapter.Error, "K8S_NO_REPOSITORY", "This service builds from source, which needs a registry to push the image to.",
			"Set spec.targets.kubernetes.repository to a registry path the cluster's nodes can pull from, such as ghcr.io/me/my-app.")
		return sv, findings
	}
	contextDir := filepath.Join(dir, svc.Path)
	sv.build = &build{contextDir: contextDir}
	if svc.Dockerfile != "" {
		path := filepath.Join(contextDir, svc.Dockerfile)
		if _, err := os.Stat(path); err != nil {
			add(adapter.Error, "K8S_DOCKERFILE_MISSING", fmt.Sprintf("Dockerfile %s not found.", filepath.Join(svc.Path, svc.Dockerfile)), "")
		}
		sv.build.dockerfile = path
		return sv, findings
	}
	generated, err := dockerfile.Generate(svc, contextDir)
	if err != nil {
		add(adapter.Error, "K8S_NEEDS_IMAGE", "No image or Dockerfile, and anyship can't generate one: "+err.Error()+".",
			"Add a Dockerfile and set services.<name>.dockerfile, or set services.<name>.image.")
		return sv, findings
	}
	sv.build.generated = generated
	add(adapter.Info, "K8S_GENERATED_DOCKERFILE", "No Dockerfile, so anyship generated one; review it in the plan's generated files.",
		"To customize the build, commit your own Dockerfile and set services.<name>.dockerfile.")
	return sv, findings
}

// The objects apply sends, as kubectl reads them. Only the fields anyship
// sets are here; everything else keeps the cluster's default.

type object struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metadata          `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
	Spec       any               `json:"spec,omitempty"`
}

type metadata struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type list struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Items      []object `json:"items"`
}

type deploymentSpec struct {
	Replicas int         `json:"replicas"`
	Selector selector    `json:"selector"`
	Template podTemplate `json:"template"`
}

type selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type podTemplate struct {
	Metadata metadata `json:"metadata"`
	Spec     podSpec  `json:"spec"`
}

type podSpec struct {
	Containers    []container `json:"containers"`
	Volumes       []volume    `json:"volumes,omitempty"`
	RestartPolicy string      `json:"restartPolicy,omitempty"`
}

type cronJobSpec struct {
	Schedule                   string      `json:"schedule"`
	TimeZone                   string      `json:"timeZone"`
	ConcurrencyPolicy          string      `json:"concurrencyPolicy"`
	SuccessfulJobsHistoryLimit int         `json:"successfulJobsHistoryLimit"`
	FailedJobsHistoryLimit     int         `json:"failedJobsHistoryLimit"`
	JobTemplate                jobTemplate `json:"jobTemplate"`
}

type jobTemplate struct {
	Spec jobSpec `json:"spec"`
}

type jobSpec struct {
	BackoffLimit int         `json:"backoffLimit"`
	Template     podTemplate `json:"template"`
}

type container struct {
	Name            string          `json:"name"`
	Image           string          `json:"image"`
	ImagePullPolicy string          `json:"imagePullPolicy"`
	Command         []string        `json:"command,omitempty"`
	Args            []string        `json:"args,omitempty"`
	Ports           []containerPort `json:"ports"`
	Env             []envVar        `json:"env,omitempty"`
	Resources       *resources      `json:"resources,omitempty"`
	StartupProbe    *probe          `json:"startupProbe,omitempty"`
	ReadinessProbe  *probe          `json:"readinessProbe,omitempty"`
	VolumeMounts    []volumeMount   `json:"volumeMounts,omitempty"`
}

type containerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type probe struct {
	HTTPGet          *httpGet    `json:"httpGet,omitempty"`
	Exec             *execAction `json:"exec,omitempty"`
	PeriodSeconds    int         `json:"periodSeconds"`
	TimeoutSeconds   int         `json:"timeoutSeconds"`
	FailureThreshold int         `json:"failureThreshold"`
}

type httpGet struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

type execAction struct {
	Command []string `json:"command"`
}

type volume struct {
	Name   string        `json:"name"`
	Secret *secretVolume `json:"secret,omitempty"`
}

type secretVolume struct {
	SecretName string `json:"secretName"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
}

type serviceSpec struct {
	Type     string            `json:"type"`
	Selector map[string]string `json:"selector"`
	Ports    []servicePort     `json:"ports"`
}

type servicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort int    `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

type ingressSpec struct {
	IngressClassName string        `json:"ingressClassName"`
	Rules            []ingressRule `json:"rules"`
}

type ingressRule struct {
	Host string      `json:"host,omitempty"`
	HTTP ingressHTTP `json:"http"`
}

type ingressHTTP struct {
	Paths []ingressPath `json:"paths"`
}

type ingressPath struct {
	Path     string         `json:"path"`
	PathType string         `json:"pathType"`
	Backend  ingressBackend `json:"backend"`
}

type ingressBackend struct {
	Service ingressService `json:"service"`
}

type ingressService struct {
	Name string `json:"name"`
	Port struct {
		Number int `json:"number"`
	} `json:"port"`
}

// portEntries is a port as the container and the Service list it: one entry
// per protocol, since tcp+udp is two.
type portEntry struct {
	name     string
	port     int
	protocol string
}

func portEntries(p spec.Port) []portEntry {
	switch p.Protocol {
	case spec.ProtocolHTTP:
		return []portEntry{{name: "http", port: p.Port, protocol: "TCP"}}
	case spec.ProtocolUDP:
		return []portEntry{{name: fmt.Sprintf("udp-%d", p.Port), port: p.Port, protocol: "UDP"}}
	case spec.ProtocolTCPUDP:
		return []portEntry{{name: fmt.Sprintf("tcp-%d", p.Port), port: p.Port, protocol: "TCP"}, {name: fmt.Sprintf("udp-%d", p.Port), port: p.Port, protocol: "UDP"}}
	}
	return []portEntry{{name: fmt.Sprintf("tcp-%d", p.Port), port: p.Port, protocol: "TCP"}}
}

func (d *planData) labels(sv service) map[string]string {
	return map[string]string{projectLabel: d.project, serviceLabel: sv.name}
}

func (d *planData) meta(name string, labels map[string]string) metadata {
	return metadata{Name: name, Namespace: d.opts.Namespace, Labels: labels}
}

// deployment is the Deployment that makes the service match the spec. Every
// setting anyship knows is in it, so a value taken out of the spec is taken
// off the pods on the next apply.
func (d *planData) deployment(sv service, img string) object {
	c := container{Name: sv.name, Image: img, ImagePullPolicy: "IfNotPresent", Args: sv.argv}
	for _, p := range sv.ports {
		for _, e := range portEntries(p) {
			c.Ports = append(c.Ports, containerPort{Name: e.name, ContainerPort: e.port, Protocol: e.protocol})
		}
	}
	if sv.moving {
		c.ImagePullPolicy = "Always"
	}
	c.Env = d.env(sv, sv.http)
	if sv.svc.CPU != 0 || sv.svc.MemoryMB() != 0 {
		size := map[string]string{}
		if sv.svc.CPU != 0 {
			size["cpu"] = strconv.FormatFloat(sv.svc.CPU, 'f', -1, 64)
		}
		if mb := sv.svc.MemoryMB(); mb != 0 {
			size["memory"] = quantity(mb)
		}
		c.Resources = &resources{Requests: size, Limits: size}
	}
	if hc := sv.svc.HealthCheck; hc != nil && (hc.Path != "" || hc.Command != "") {
		startup := probe{PeriodSeconds: probePeriod, TimeoutSeconds: probeTimeout, FailureThreshold: probeFailures}
		if hc.Path != "" {
			startup.HTTPGet = &httpGet{Path: hc.Path, Port: sv.port}
		} else {
			startup.Exec = &execAction{Command: []string{"sh", "-c", hc.Command}}
		}
		readiness := startup
		readiness.FailureThreshold = readinessFailures
		c.StartupProbe, c.ReadinessProbe = &startup, &readiness
	}
	// Each secret is one file, mounted on its own: a volume over the whole
	// of /run/secrets would be read-only and keep the kubelet from putting
	// the service account token under /var/run/secrets, which is the same
	// place in most images.
	pod := podSpec{}
	annotations := map[string]string{}
	pod.Volumes, c.VolumeMounts = d.secretMounts(sv)
	if len(sv.svc.Secrets) > 0 {
		annotations[secretsAnnotation] = d.versions
	}
	pod.Containers = []container{c}
	template := podTemplate{Metadata: metadata{Labels: d.labels(sv)}, Spec: pod}
	if sv.moving {
		annotations[rolloutAnnotation] = d.rollout
	}
	if len(annotations) > 0 {
		template.Metadata.Annotations = annotations
	}
	return object{APIVersion: "apps/v1", Kind: "Deployment", Metadata: d.meta(sv.object, d.labels(sv)),
		Spec: deploymentSpec{Replicas: replicas(sv.svc), Selector: selector{MatchLabels: d.labels(sv)}, Template: template}}
}

// env is the service's environment, with $PORT when asked and references
// to other services resolved to their addresses in the cluster.
func (d *planData) env(sv service, withPort bool) []envVar {
	env := map[string]string{}
	if withPort {
		env["PORT"] = strconv.Itoa(sv.port)
	}
	for k, v := range sv.svc.Env {
		env[k] = spec.ExpandServiceURLs(v, d.serviceURL)
	}
	var vars []envVar
	for _, k := range sortedKeys(env) {
		vars = append(vars, envVar{Name: k, Value: env[k]})
	}
	return vars
}

// secretMounts puts each of the service's secrets at /run/secrets/<NAME>.
func (d *planData) secretMounts(sv service) ([]volume, []volumeMount) {
	var volumes []volume
	var mounts []volumeMount
	for _, name := range sv.svc.Secrets {
		object := secretName(d.project, name)
		volumes = append(volumes, volume{Name: object, Secret: &secretVolume{SecretName: object}})
		mounts = append(mounts, volumeMount{Name: object, MountPath: secretsDir + "/" + name, SubPath: name, ReadOnly: true})
	}
	return volumes, mounts
}

// cronJob is the CronJob for one cron entry: a curl container calling the
// service's path, or the service's own image running the command. Its pods
// carry cronLabel rather than serviceLabel, so the Service never sends them
// traffic. Runs don't overlap, and a run that fails is retried twice.
func (d *planData) cronJob(sv service, job cronJob, img string) object {
	labels := map[string]string{projectLabel: d.project, cronLabel: sv.name}
	c := container{Name: sv.name, ImagePullPolicy: "IfNotPresent"}
	pod := podSpec{RestartPolicy: "Never"}
	if job.path != "" {
		c.Image = d.opts.cronImage()
		c.Command = []string{"curl"}
		c.Args = []string{"-fsS", "-X", job.method, d.serviceURL(sv.name) + job.path}
	} else {
		c.Image = img
		if sv.moving {
			c.ImagePullPolicy = "Always"
		}
		c.Args = job.argv
		c.Env = d.env(sv, false)
		pod.Volumes, c.VolumeMounts = d.secretMounts(sv)
	}
	pod.Containers = []container{c}
	return object{APIVersion: "batch/v1", Kind: "CronJob", Metadata: d.meta(job.id, labels),
		Spec: cronJobSpec{Schedule: job.schedule, TimeZone: "Etc/UTC", ConcurrencyPolicy: "Forbid",
			SuccessfulJobsHistoryLimit: 3, FailedJobsHistoryLimit: 3,
			JobTemplate: jobTemplate{Spec: jobSpec{BackoffLimit: 2, Template: podTemplate{Metadata: metadata{Labels: labels}, Spec: pod}}}}}
}

func (d *planData) service(sv service) object {
	spec := serviceSpec{Type: "ClusterIP", Selector: d.labels(sv)}
	if sv.lb {
		spec.Type = "LoadBalancer"
	}
	for _, p := range sv.ports {
		for _, e := range portEntries(p) {
			spec.Ports = append(spec.Ports, servicePort{Name: e.name, Port: e.port, TargetPort: e.port, Protocol: e.protocol})
		}
	}
	return object{APIVersion: "v1", Kind: "Service", Metadata: d.meta(sv.object, d.labels(sv)), Spec: spec}
}

// ingress routes the service's domains, or any host, to its HTTP port. It
// carries no TLS section: certificates are the ingress controller's to add.
func (d *planData) ingress(sv service) object {
	path := ingressPath{Path: "/", PathType: "Prefix"}
	path.Backend.Service.Name = sv.object
	path.Backend.Service.Port.Number = sv.port
	spec := ingressSpec{IngressClassName: d.opts.IngressClass}
	if len(sv.svc.Domains) == 0 {
		spec.Rules = []ingressRule{{HTTP: ingressHTTP{Paths: []ingressPath{path}}}}
	}
	for _, host := range sv.svc.Domains {
		spec.Rules = append(spec.Rules, ingressRule{Host: host, HTTP: ingressHTTP{Paths: []ingressPath{path}}})
	}
	return object{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Metadata: d.meta(sv.object, d.labels(sv)), Spec: spec}
}

func (d *planData) secret(sc secret, value string) object {
	return object{APIVersion: "v1", Kind: "Secret", Metadata: d.meta(sc.object, map[string]string{projectLabel: d.project}),
		Type: "Opaque", Data: map[string]string{sc.name: base64.StdEncoding.EncodeToString([]byte(value))}}
}

// manifests is the file apply sends to kubectl: a List of every service's
// Deployment and Service, in deploy order. It is JSON, which is YAML.
func manifests(d *planData) ([]byte, error) {
	l := list{APIVersion: "v1", Kind: "List"}
	for _, sv := range d.services {
		l.Items = append(l.Items, d.deployment(sv, sv.imageOrPlaceholder(d.opts)), d.service(sv))
		if sv.ingress {
			l.Items = append(l.Items, d.ingress(sv))
		}
		for _, job := range sv.jobs {
			l.Items = append(l.Items, d.cronJob(sv, job, sv.imageOrPlaceholder(d.opts)))
		}
	}
	return render(l, "# Generated by anyship from anyship.yaml. Edit anyship.yaml instead; this file is overwritten.\n")
}

func render(v any, header string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quantity writes megabytes the way Kubernetes takes them: "512Mi", "4Gi".
func quantity(mb int) string {
	if mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + "Gi"
	}
	return strconv.Itoa(mb) + "Mi"
}

func decodeOptions(raw json.RawMessage) (*Options, error) {
	if len(raw) == 0 {
		return nil, errors.New("namespace is required")
	}
	opts := &Options{}
	if err := adapter.DecodeOptions(raw, opts); err != nil {
		return nil, err
	}
	switch {
	case opts.Namespace == "":
		return nil, errors.New("namespace is required")
	case !dnsLabelRe.MatchString(opts.Namespace):
		return nil, fmt.Errorf("namespace %q is not a valid namespace name (lowercase letters, digits and dashes)", opts.Namespace)
	case strings.ContainsAny(opts.Context, " \t\n"):
		return nil, fmt.Errorf("context %q is not a kubeconfig context name", opts.Context)
	case opts.Repository != "" && (!repositoryRe.MatchString(opts.Repository) || strings.Contains(opts.Repository, "@")):
		return nil, fmt.Errorf("repository %q is not a registry path such as ghcr.io/me/my-app (lowercase, no tag)", opts.Repository)
	case opts.Platform != "" && !platformRe.MatchString(opts.Platform):
		return nil, fmt.Errorf("platform %q is not a platform such as linux/amd64 or linux/arm64", opts.Platform)
	case opts.IngressClass != "" && !dnsSubdomainRe.MatchString(opts.IngressClass):
		return nil, fmt.Errorf("ingressClass %q is not an IngressClass name", opts.IngressClass)
	case opts.CronImage != "" && (strings.ContainsAny(opts.CronImage, " \t\n") || strings.HasPrefix(opts.CronImage, "-")):
		return nil, fmt.Errorf("cronImage %q is not an image reference", opts.CronImage)
	}
	return opts, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
