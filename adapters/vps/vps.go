// Package vps deploys a spec to a Linux server over SSH with Docker Compose.
//
// It renders a compose.yaml from the spec, uploads it with the build contexts
// and secret values through the system ssh client, and runs `docker compose up`
// on the host. Using the system ssh means the deployer's ~/.ssh/config, agent,
// jump hosts and known_hosts verification all apply as usual.
package vps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/spec"
)

// Name is the target name used in `--target` and `targets.vps`.
const Name = "vps"

// Options is the targets.vps block of a spec.
type Options struct {
	// Host is the ssh destination: a hostname, user@host, or a ~/.ssh/config alias.
	Host         string `json:"host"`
	Port         int    `json:"port,omitempty"`
	IdentityFile string `json:"identityFile,omitempty"`
	// Dir is the deployment directory on the host, relative to the login
	// user's home unless absolute. Defaults to anyship/<spec name>.
	Dir string `json:"dir,omitempty"`
	// Sudo runs docker through `sudo -n` for users outside the docker group.
	Sudo bool `json:"sudo,omitempty"`
}

// deployDir is the deployment directory on the host for a project.
func (o Options) deployDir(project string) string {
	if o.Dir != "" {
		return o.Dir
	}
	return "anyship/" + project
}

// docker is the docker command prefix on the host.
func (o Options) docker() string {
	if o.Sudo {
		return "sudo -n docker"
	}
	return "docker"
}

// planData is handed from Plan to Apply.
type planData struct {
	opts     Options
	project  string
	dir      string
	compose  []byte
	contexts map[string]string
	// extra holds generated files to upload, keyed by archive path.
	extra map[string][]byte
	// generated secrets are created on the host when missing; required ones
	// must come from the deployer's environment or a previous deploy.
	generated []string
	required  []string
}

type Adapter struct{}

var (
	_ adapter.Adapter   = (*Adapter)(nil)
	_ adapter.LogReader = (*Adapter)(nil)
)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return Name }

func (*Adapter) Description() string {
	return "Any Linux server with Docker, over SSH (Docker Compose)"
}

func (a *Adapter) Plan(_ context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	plan := &adapter.Plan{Target: Name}

	opts, err := decodeOptions(s.Targets[Name])
	if err != nil {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level:   adapter.Error,
			Code:    "VPS_BAD_OPTIONS",
			Message: "targets.vps: " + err.Error(),
			Hint:    `Set targets.vps.host to an ssh destination, e.g. {"vps": {"host": "deploy@203.0.113.10"}}.`,
		})
		return plan, nil
	}
	dir := opts.deployDir(s.Name)

	compose := composeFile{Name: s.Name, Services: map[string]composeService{}}
	contexts := map[string]string{}
	extra := map[string][]byte{}
	var reviewFiles []adapter.File
	published := map[string]string{} // "8080/tcp" → service publishing it
	usedSecrets := map[string]bool{}
	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		c := checkService(name, svc, s, env.Dir)
		plan.Findings = append(plan.Findings, c.findings...)

		for _, m := range portMappings(svc) {
			if other, taken := published[m.key()]; taken {
				plan.Findings = append(plan.Findings, adapter.Finding{
					Level:   adapter.Error,
					Code:    "VPS_PORT_CONFLICT",
					Service: name,
					Message: fmt.Sprintf("Services %q and %q both publish host port %s.", other, name, m.key()),
				})
				continue
			}
			published[m.key()] = name
		}

		var build *composeBuild
		switch {
		case svc.Dockerfile != "":
			build = &composeBuild{Context: contextDir(name), Dockerfile: svc.Dockerfile}
		case c.generated != nil:
			build = &composeBuild{Context: contextDir(name), Dockerfile: dockerfile.Filename}
			extra[path.Join(contextDir(name), dockerfile.Filename)] = c.generated.Dockerfile
			extra[path.Join(contextDir(name), dockerfile.IgnoreFilename)] = c.generated.Ignore
			reviewFiles = append(reviewFiles, adapter.File{Path: filepath.Join(env.OutDir, name+".Dockerfile"), Contents: c.generated.Dockerfile})
		}
		if build != nil {
			contexts[name] = filepath.Join(env.Dir, svc.Path)
		}
		compose.Services[name] = renderService(svc, c.argv, build)
		for _, v := range svc.Volumes {
			if compose.Volumes == nil {
				compose.Volumes = map[string]struct{}{}
			}
			compose.Volumes[v.Name] = struct{}{}
		}
		for _, secret := range svc.Secrets {
			usedSecrets[secret] = true
		}
	}
	if adapter.HasErrors(plan.Findings) {
		return plan, nil
	}

	data := &planData{opts: *opts, project: s.Name, dir: dir, contexts: contexts, extra: extra}
	for _, secret := range sortedKeys(usedSecrets) {
		if compose.Secrets == nil {
			compose.Secrets = map[string]composeSecret{}
		}
		compose.Secrets[secret] = composeSecret{File: "./secrets/" + secret}
		action := adapter.Action{Op: adapter.OpNote, Kind: "secret", Name: secret}
		if s.Secrets[secret].Generate != "" {
			data.generated = append(data.generated, secret)
			action.Detail = "generated on the host on first deploy, then kept (set $" + secret + " to override)"
		} else {
			data.required = append(data.required, secret)
			action.Detail = "read from $" + secret + " when set; otherwise the value from the previous deploy is kept"
		}
		plan.Actions = append(plan.Actions, action)
	}

	if data.compose, err = renderCompose(compose); err != nil {
		return nil, err
	}

	upload := "compose.yaml"
	if n := len(contexts); n > 0 {
		upload += fmt.Sprintf(" and %d build context(s)", n)
	}
	plan.Actions = append(plan.Actions,
		adapter.Action{Op: adapter.OpRun, Kind: "upload", Name: opts.Host + ":" + dir, Detail: upload},
		adapter.Action{Op: adapter.OpDeploy, Kind: "compose", Name: s.Name, Detail: "docker compose up -d --build on " + opts.Host},
	)
	plan.Files = append(plan.Files, adapter.File{Path: filepath.Join(env.OutDir, "compose.yaml"), Contents: data.compose})
	plan.Files = append(plan.Files, reviewFiles...)
	plan.Data = data
	return plan, nil
}

// Apply runs the preflight checks, then uploads and starts the project.
// With env.DryRun it stops after the checks.
func (a *Adapter) Apply(ctx context.Context, plan *adapter.Plan, s *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	if adapter.HasErrors(plan.Findings) {
		return failed("The plan has errors; fix them and plan again."), nil
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
		rel, _ := filepath.Rel(env.Dir, f.Path)
		env.Logf("wrote %s", filepath.ToSlash(rel))
	}

	host := data.opts.Host
	docker := data.opts.docker()
	ssh := func(stdin io.Reader, remote string) error {
		args := append(sshArgs(data.opts), remote)
		return env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir, Stdin: stdin}, "ssh", args...)
	}

	env.Logf("$ ssh %s (preflight checks)", host)
	var report bytes.Buffer
	preflightOpts := adapter.ExecOptions{Dir: env.Dir, Stdin: bytes.NewReader(data.compose), Stdout: &report}
	if err := env.Exec(ctx, preflightOpts, "ssh", append(sshArgs(data.opts), preflightScript(data, docker))...); err != nil {
		return failed(fmt.Sprintf("Cannot run the preflight checks on %s (%v). Check that `ssh %s` works without a password prompt.", host, err, host)), nil
	}
	checks := parsePreflight(report.Bytes()).findings(s, host)
	result := func(ok bool, message string) *adapter.Result {
		return &adapter.Result{OK: ok, Findings: checks, Messages: []string{message}}
	}
	if adapter.HasErrors(checks) {
		return result(false, "Preflight checks failed; nothing was uploaded or started."), nil
	}
	if env.DryRun {
		return result(true, fmt.Sprintf("Dry run: preflight checks passed on %s; nothing was uploaded or started.", host)), nil
	}

	b := &bundle{compose: data.compose, contexts: data.contexts, extra: data.extra, secrets: map[string]string{}}
	for _, name := range slices.Concat(data.generated, data.required) {
		if value, ok := env.LookupEnv(name); ok && value != "" {
			b.secrets[name] = value
		}
	}
	q := shellwords.Quote(data.dir)
	upload := fmt.Sprintf("mkdir -p %s && rm -rf %s/src && tar -xf - -C %s", q, q, q)
	env.Logf("$ ssh %s %s", host, upload)
	if err := streamTo(func(r io.Reader) error { return ssh(r, upload) }, b.write); err != nil {
		var packErr *packError
		if errors.As(err, &packErr) {
			return nil, packErr.err
		}
		return result(false, fmt.Sprintf("Upload to %s failed: %v", host, err)), nil
	}

	env.Logf("$ ssh %s %s compose up -d --build", host, docker)
	if err := ssh(nil, deployScript(data, docker)); err != nil {
		return result(false, fmt.Sprintf("docker compose up failed on %s: %v", host, err)), nil
	}
	return result(true, fmt.Sprintf("Deployed %s to %s:%s.", data.project, host, data.dir)), nil
}

// defaultLogTail keeps `anyship logs` from dumping a long-running service's
// entire history.
const defaultLogTail = 100

// Logs runs `docker compose logs` for the deployed project on the host.
func (a *Adapter) Logs(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return fmt.Errorf("targets.vps: %w", err)
	}
	dir := o.deployDir(s.Name)

	tail := opts.Tail
	if tail == 0 {
		tail = defaultLogTail
	}
	remote := fmt.Sprintf("cd %s && %s compose -p %s -f compose.yaml logs --tail %d", shellwords.Quote(dir), o.docker(), s.Name, tail)
	if opts.Since != "" {
		remote += " --since " + shellwords.Quote(opts.Since)
	}
	if opts.Timestamps {
		remote += " --timestamps"
	}
	if opts.Follow {
		remote += " --follow"
	}
	if opts.Service != "" {
		remote += " " + shellwords.Quote(opts.Service)
	}

	env.Logf("$ ssh %s %s", o.Host, remote)
	args := append(sshArgs(*o), remote)
	if err := env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir}, "ssh", args...); err != nil {
		return fmt.Errorf("reading logs from %s failed (%w); has %s been deployed there with `anyship apply -t vps`?", o.Host, err, s.Name)
	}
	return nil
}

type packError struct{ err error }

func (e *packError) Error() string { return "packing the upload: " + e.err.Error() }

// streamTo runs consume with a reader fed by produce. A failure to produce
// is reported as *packError; a consumer that exits early stops the producer.
func streamTo(consume func(io.Reader) error, produce func(io.Writer) error) error {
	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := produce(pw)
		pw.CloseWithError(err)
		produced <- err
	}()
	consumeErr := consume(pr)
	pr.CloseWithError(io.ErrClosedPipe) // unblocks the producer if the consumer stopped reading
	produceErr := <-produced
	if consumeErr != nil {
		return consumeErr
	}
	if produceErr != nil {
		return &packError{produceErr}
	}
	return nil
}

// deployScript runs on the host after the upload: it fills in secrets and
// brings the project up. Secret names and the project name are validated by
// the spec, so only the directory needs quoting.
func deployScript(d *planData, docker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\ncd %s\n", shellwords.Quote(d.dir))
	if len(d.generated)+len(d.required) > 0 {
		b.WriteString("mkdir -p secrets\nchmod 700 secrets\n")
	}
	for _, name := range d.generated {
		fmt.Fprintf(&b, "if [ ! -s secrets/%[1]s ]; then head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n' > secrets/%[1]s; echo 'anyship: generated secret %[1]s'; fi\n", name)
	}
	for _, name := range d.required {
		fmt.Fprintf(&b, "if [ ! -s secrets/%[1]s ]; then echo 'anyship: secret %[1]s has no value; export %[1]s locally and apply again' >&2; exit 1; fi\n", name)
	}
	compose := fmt.Sprintf("%s compose -p %s -f compose.yaml", docker, d.project)
	fmt.Fprintf(&b, "%s up -d --build --remove-orphans\n%s ps\n", compose, compose)
	return b.String()
}

// sshArgs returns the ssh arguments up to and including the destination.
// BatchMode makes unknown host keys and password prompts fail instead of
// hang, and ConnectTimeout does the same for unreachable hosts.
func sshArgs(o Options) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
	if o.Port != 0 {
		args = append(args, "-p", strconv.Itoa(o.Port))
	}
	if o.IdentityFile != "" {
		args = append(args, "-i", o.IdentityFile)
	}
	return append(args, "--", o.Host)
}

// checked is what checkService learned about one service.
type checked struct {
	findings []adapter.Finding
	// argv is the split start command, nil to keep the image's own.
	argv []string
	// generated is set when anyship wrote the service's Dockerfile.
	generated *dockerfile.Result
}

func checkService(name string, svc *spec.Service, s *spec.Spec, dir string) checked {
	var c checked
	add := func(level adapter.Level, code, message, hint string) {
		c.findings = append(c.findings, adapter.Finding{Level: level, Code: code, Service: name, Message: message, Hint: hint})
	}

	if svc.Image == "" && svc.Dockerfile == "" {
		generated, err := dockerfile.Generate(svc, filepath.Join(dir, svc.Path))
		if err != nil {
			add(adapter.Error, "VPS_NEEDS_IMAGE",
				"Services run as containers on a VPS. This one has no image or Dockerfile, and anyship can't generate one: "+err.Error()+".",
				"Add a Dockerfile and set services.<name>.dockerfile.")
		} else {
			c.generated = generated
			add(adapter.Info, "VPS_GENERATED_DOCKERFILE",
				"No Dockerfile, so anyship generated one; review it in the generated files below.",
				"To customize the build, commit your own Dockerfile and set services.<name>.dockerfile.")
		}
	}
	if svc.Dockerfile != "" {
		if _, err := os.Stat(filepath.Join(dir, svc.Path, svc.Dockerfile)); err != nil {
			add(adapter.Error, "VPS_DOCKERFILE_MISSING", fmt.Sprintf("Dockerfile %s not found.", filepath.Join(svc.Path, svc.Dockerfile)), "")
		}
	}

	if svc.Start != "" {
		var err error
		if c.argv, err = shellwords.Split(svc.Start); err != nil {
			add(adapter.Error, "VPS_BAD_START", "start "+err.Error()+".", "")
		}
	} else if svc.Entry != "" {
		add(adapter.Info, "VPS_ENTRY_IGNORED", "entry is only used by edge runtimes; the image's own command runs on a VPS.", "")
	}

	if len(svc.Cron) > 0 {
		add(adapter.Error, "VPS_CRON_UNSUPPORTED",
			"Cron schedules are not supported on the VPS target yet.",
			"Run a scheduler such as supercronic inside the container for now.")
	}
	for _, resourceName := range svc.Uses {
		resource := s.Resources[resourceName]
		if resource.External {
			add(adapter.Info, "VPS_EXTERNAL_RESOURCE",
				fmt.Sprintf("Resource %q is external; pass its connection settings through env or secrets.", resourceName), "")
			continue
		}
		add(adapter.Error, "VPS_RESOURCE_UNSUPPORTED",
			fmt.Sprintf("anyship does not provision %s on a VPS yet (resource %q).", resource.Type, resourceName),
			`Add it as a service (e.g. image "postgres:17" with a volume), or mark the resource external.`)
	}

	mappings := portMappings(svc)
	if svc.Replicas > 1 && len(mappings) > 0 {
		add(adapter.Error, "VPS_REPLICAS_WITH_PORTS",
			fmt.Sprintf("%d replicas cannot all publish the same host ports.", svc.Replicas),
			"Use one replica, or keep these ports internal behind a load balancer service.")
	}
	if svc.HealthCheck != nil && svc.HealthCheck.Path != "" && svc.HealthCheck.Command == "" {
		add(adapter.Warning, "VPS_HEALTH_PATH_IGNORED",
			"healthCheck.path is not supported on the VPS target and is ignored.",
			"Use healthCheck.command, e.g. \"wget -qO- http://localhost:8080/health\".")
	}
	if len(svc.Volumes) > 0 {
		var sizes []string
		for _, v := range svc.Volumes {
			sizes = append(sizes, fmt.Sprintf("%s %s %s", v.Name, v.Size, v.Class))
		}
		add(adapter.Info, "VPS_VOLUME_SIZE",
			"Docker volumes don't enforce size or disk class; make sure the host has room for: "+strings.Join(sizes, ", ")+".", "")
	}
	for _, m := range mappings {
		if m.http {
			add(adapter.Info, "VPS_NO_TLS",
				fmt.Sprintf("Port %d is served directly over plain HTTP; domains and HTTPS are planned.", m.host), "")
		}
	}
	return c
}

// decodeOptions reads targets.vps strictly, so typos surface as findings.
func decodeOptions(raw json.RawMessage) (*Options, error) {
	if len(raw) == 0 {
		return nil, errors.New("host is required")
	}
	opts := &Options{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(opts); err != nil {
		return nil, err
	}
	switch {
	case opts.Host == "":
		return nil, errors.New("host is required")
	case strings.HasPrefix(opts.Host, "-") || strings.ContainsAny(opts.Host, " \t\r\n"):
		return nil, fmt.Errorf("host %q is not a valid ssh destination", opts.Host)
	case opts.Port < 0 || opts.Port > 65535:
		return nil, errors.New("port must be between 1 and 65535")
	case strings.HasPrefix(opts.Dir, "~"):
		return nil, errors.New(`dir must not start with "~"; relative paths are already under the login user's home`)
	case strings.ContainsAny(opts.Dir, "\r\n\x00"):
		return nil, errors.New("dir must be a single-line path")
	case opts.Dir != "" && !ownDir(opts.Dir):
		// destroy --volumes removes this directory, so it must be one anyship owns.
		return nil, fmt.Errorf("dir %q must be a subdirectory, not the home directory, / or a parent", opts.Dir)
	}
	return opts, nil
}

// ownDir reports whether dir is safe to delete as a whole: a directory below
// the login user's home, or an absolute path at least two levels deep (so
// "/srv/app", never "/" or "/srv").
func ownDir(dir string) bool {
	clean := path.Clean(dir)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	if path.IsAbs(clean) {
		return strings.Count(clean, "/") >= 2
	}
	return true
}

func failed(message string) *adapter.Result {
	return &adapter.Result{Messages: []string{message}}
}
