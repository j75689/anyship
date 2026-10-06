// Package cloudflare deploys a spec to Cloudflare Workers.
//
// It renders a wrangler.jsonc from the spec and deploys it with
// `wrangler deploy`. Cloudflare can't host everything a spec can describe
// (disks, raw TCP/UDP, container images, Node-only code), so Plan reports each
// unmet need as an error instead of quietly dropping it.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// DefaultCompatibilityDate is used unless targets.cloudflare.compatibilityDate
// overrides it. It is a constant so rendering stays deterministic.
const DefaultCompatibilityDate = "2026-09-01"

// Options is the targets.cloudflare block of a spec.
type Options struct {
	// Name is the Worker name; defaults to the spec name.
	Name              string `json:"name,omitempty"`
	CompatibilityDate string `json:"compatibilityDate,omitempty"`
	AccountID         string `json:"accountId,omitempty"`
	// Domains are custom domains attached to the Worker. When set they replace
	// the service's own services.<name>.domains, so a Cloudflare deploy can use
	// a different set of hosts than the platform-neutral spec asks for.
	Domains []string `json:"domains,omitempty"`
	// SPA serves index.html for unknown paths.
	SPA bool `json:"spa,omitempty"`
	// Bindings maps spec resource names to existing Cloudflare resources: the
	// D1 database id, KV namespace id, Hyperdrive config id, or R2 bucket name.
	Bindings map[string]Binding `json:"bindings,omitempty"`
}

type Binding struct {
	ID string `json:"id"`
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type bindingRule struct {
	product string
	// createCommand creates the resource and prints its id.
	createCommand func(name string) string
	// needsID is false for R2, whose buckets are addressed by name.
	needsID bool
}

var bindingRules = map[spec.ResourceType]*bindingRule{
	spec.ResourceSQLite: {product: "D1", needsID: true, createCommand: func(n string) string { return "npx wrangler d1 create " + n }},
	spec.ResourceKV:     {product: "KV", needsID: true, createCommand: func(n string) string { return "npx wrangler kv namespace create " + n }},
	spec.ResourceBucket: {product: "R2", needsID: false, createCommand: func(n string) string { return "npx wrangler r2 bucket create " + n }},
	spec.ResourcePostgres: {product: "Hyperdrive", needsID: true, createCommand: func(n string) string {
		return fmt.Sprintf(`npx wrangler hyperdrive create %s --connection-string="postgres://..."`, n)
	}},
	spec.ResourceMySQL: {product: "Hyperdrive", needsID: true, createCommand: func(n string) string {
		return fmt.Sprintf(`npx wrangler hyperdrive create %s --connection-string="mysql://..."`, n)
	}},
	spec.ResourceRedis: nil,
}

// wranglerConfig mirrors the subset of wrangler.jsonc anyship renders. Field
// order here is the order in the generated file.
type wranglerConfig struct {
	Name               string            `json:"name"`
	CompatibilityDate  string            `json:"compatibility_date"`
	CompatibilityFlags []string          `json:"compatibility_flags"`
	AccountID          string            `json:"account_id,omitempty"`
	Main               string            `json:"main,omitempty"`
	Assets             *assetsConfig     `json:"assets,omitempty"`
	Vars               map[string]string `json:"vars,omitempty"`
	Triggers           *triggersConfig   `json:"triggers,omitempty"`
	Routes             []routeConfig     `json:"routes,omitempty"`
	D1Databases        []d1Binding       `json:"d1_databases,omitempty"`
	KVNamespaces       []idBinding       `json:"kv_namespaces,omitempty"`
	R2Buckets          []r2Binding       `json:"r2_buckets,omitempty"`
	Hyperdrive         []idBinding       `json:"hyperdrive,omitempty"`
}

type assetsConfig struct {
	Directory        string `json:"directory"`
	NotFoundHandling string `json:"not_found_handling,omitempty"`
}

type triggersConfig struct {
	Crons []string `json:"crons"`
}

type routeConfig struct {
	Pattern      string `json:"pattern"`
	CustomDomain bool   `json:"custom_domain"`
}

type d1Binding struct {
	Binding      string `json:"binding"`
	DatabaseName string `json:"database_name"`
	DatabaseID   string `json:"database_id"`
}

type idBinding struct {
	Binding string `json:"binding"`
	ID      string `json:"id"`
}

type r2Binding struct {
	Binding    string `json:"binding"`
	BucketName string `json:"bucket_name"`
}

// planData is handed from Plan to Apply.
type planData struct {
	configPath   string
	serviceDir   string
	buildCommand string
}

type Adapter struct{}

var (
	_ adapter.Adapter   = (*Adapter)(nil)
	_ adapter.LogReader = (*Adapter)(nil)
)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return "cloudflare" }

func (*Adapter) Description() string {
	return "Cloudflare Workers (edge handlers and static assets) via wrangler"
}

func (a *Adapter) Plan(_ context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	plan := &adapter.Plan{Target: a.Name()}
	addError := func(code, service, message, hint string) {
		plan.Findings = append(plan.Findings, adapter.Finding{Level: adapter.Error, Code: code, Service: service, Message: message, Hint: hint})
	}

	opts, err := decodeOptions(s.Targets["cloudflare"])
	if err != nil {
		addError("CF_BAD_OPTIONS", "", "spec.targets.cloudflare: "+err.Error(), "")
		return plan, nil
	}

	names := s.ServiceNames()
	if len(names) != 1 {
		addError("CF_MULTI_SERVICE", "",
			fmt.Sprintf("The Cloudflare target deploys one Worker, but this spec has %d services (%s).", len(names), strings.Join(names, ", ")),
			"Split the spec per service, or use a container/VPS target.")
	}
	for _, name := range names {
		plan.Findings = append(plan.Findings, checkService(name, s.Services[name], s)...)
	}
	if adapter.HasErrors(plan.Findings) {
		return plan, nil
	}

	serviceName := names[0]
	svc := s.Services[serviceName]
	workerName := cmp(opts.Name, s.Name)
	serviceDir := filepath.Join(env.Dir, svc.Path)
	configPath := filepath.Join(env.OutDir, "wrangler.jsonc")
	// wrangler resolves paths relative to the config file, which lives in OutDir.
	fromConfig := func(target string) string {
		rel, err := filepath.Rel(env.OutDir, target)
		if err != nil {
			return filepath.ToSlash(target)
		}
		return filepath.ToSlash(rel)
	}

	config := wranglerConfig{
		Name:               workerName,
		CompatibilityDate:  cmp(opts.CompatibilityDate, DefaultCompatibilityDate),
		CompatibilityFlags: []string{"nodejs_compat"},
		AccountID:          opts.AccountID,
	}

	if svc.Kind == spec.KindServer {
		config.Main = fromConfig(filepath.Join(serviceDir, svc.Entry))
	} else {
		output := "."
		if svc.Build != nil && svc.Build.Output != "" {
			output = svc.Build.Output
		}
		assetsDir := filepath.Join(serviceDir, output)
		config.Assets = &assetsConfig{Directory: fromConfig(assetsDir)}
		if opts.SPA {
			config.Assets.NotFoundHandling = "single-page-application"
		}
		if assetsDir == serviceDir {
			plan.Findings = append(plan.Findings, adapter.Finding{
				Level:   adapter.Warning,
				Code:    "CF_ASSETS_ROOT",
				Service: serviceName,
				Message: "Static assets are served from the service root, so every file in it will be uploaded.",
				Hint:    "Set services.<name>.build.output to the directory that holds only the built site.",
			})
		}
	}

	if len(svc.Env) > 0 {
		config.Vars = svc.Env
	}
	for _, c := range svc.Cron {
		if config.Triggers == nil {
			config.Triggers = &triggersConfig{}
		}
		config.Triggers.Crons = append(config.Triggers.Crons, c.Schedule)
	}
	// targets.cloudflare.domains overrides the service's domains, so this target
	// can be deployed to other hosts than the rest of the spec.
	domains := svc.Domains
	if len(opts.Domains) > 0 {
		domains = opts.Domains
		if len(svc.Domains) > 0 {
			plan.Findings = append(plan.Findings, adapter.Finding{
				Level: adapter.Info, Code: "CF_DOMAIN_OVERRIDE", Service: serviceName,
				Message: fmt.Sprintf("targets.cloudflare.domains (%s) replaces services.%s.domains (%s).",
					strings.Join(opts.Domains, ", "), serviceName, strings.Join(svc.Domains, ", ")),
				Hint: "Remove targets.cloudflare.domains to attach the service's own domains.",
			})
		}
	}
	for _, domain := range domains {
		config.Routes = append(config.Routes, routeConfig{Pattern: domain, CustomDomain: true})
	}

	for _, resourceName := range svc.Uses {
		resource := s.Resources[resourceName]
		rule := bindingRules[resource.Type]
		bindingName := strings.ToUpper(strings.ReplaceAll(resourceName, "-", "_"))
		cfName := workerName + "-" + resourceName
		id := opts.Bindings[resourceName].ID

		if rule.needsID && id == "" {
			addError("CF_MISSING_RESOURCE_ID", serviceName,
				fmt.Sprintf("Resource %q (%s) needs an existing %s id.", resourceName, resource.Type, rule.product),
				fmt.Sprintf("Run `%s` and set targets.cloudflare.bindings.%s.id.", rule.createCommand(cfName), resourceName))
			plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpCreate, Kind: rule.product, Name: cfName, Detail: rule.createCommand(cfName)})
			continue
		}

		switch resource.Type {
		case spec.ResourceSQLite:
			config.D1Databases = append(config.D1Databases, d1Binding{Binding: bindingName, DatabaseName: cfName, DatabaseID: id})
		case spec.ResourceKV:
			config.KVNamespaces = append(config.KVNamespaces, idBinding{Binding: bindingName, ID: id})
		case spec.ResourcePostgres, spec.ResourceMySQL:
			config.Hyperdrive = append(config.Hyperdrive, idBinding{Binding: bindingName, ID: id})
		case spec.ResourceBucket:
			config.R2Buckets = append(config.R2Buckets, r2Binding{Binding: bindingName, BucketName: cmp(id, cfName)})
			if id == "" {
				plan.Findings = append(plan.Findings, adapter.Finding{
					Level:   adapter.Info,
					Code:    "CF_R2_BUCKET",
					Service: serviceName,
					Message: fmt.Sprintf("R2 bucket %q must exist before deploying.", cfName),
					Hint:    fmt.Sprintf("Run `%s`, or set targets.cloudflare.bindings.%s.id to an existing bucket name.", rule.createCommand(cfName), resourceName),
				})
			}
		}
	}
	// Missing resource ids are errors; the create actions above say what to run.
	if adapter.HasErrors(plan.Findings) {
		return plan, nil
	}

	for _, secret := range svc.Secrets {
		plan.Actions = append(plan.Actions, adapter.Action{
			Op: adapter.OpNote, Kind: "secret", Name: secret,
			Detail: fmt.Sprintf("set it once with: npx wrangler secret put %s --name %s", secret, workerName),
		})
	}
	buildCommand := ""
	if svc.Build != nil && svc.Build.Command != "" {
		buildCommand = svc.Build.Command
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpRun, Kind: "build", Name: serviceName, Detail: buildCommand})
	}
	relConfig, _ := filepath.Rel(env.Dir, configPath)
	plan.Actions = append(plan.Actions, adapter.Action{
		Op: adapter.OpDeploy, Kind: "worker", Name: workerName, Detail: "wrangler deploy --config " + filepath.ToSlash(relConfig),
	})

	contents, err := render(config)
	if err != nil {
		return nil, err
	}
	plan.Files = append(plan.Files, adapter.File{Path: configPath, Contents: contents})
	plan.Data = &planData{configPath: configPath, serviceDir: serviceDir, buildCommand: buildCommand}
	return plan, nil
}

func (a *Adapter) Apply(ctx context.Context, plan *adapter.Plan, _ *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	if adapter.HasErrors(plan.Findings) {
		return &adapter.Result{Messages: []string{"The plan has errors; fix them and plan again."}}, nil
	}
	data, ok := plan.Data.(*planData)
	if !ok {
		return nil, fmt.Errorf("plan was not produced by the %s adapter", a.Name())
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

	if data.buildCommand != "" {
		env.Logf("$ %s", data.buildCommand)
		if err := env.Exec(ctx, adapter.ExecOptions{Dir: data.serviceDir, Shell: true}, data.buildCommand); err != nil {
			return &adapter.Result{Messages: []string{"Build failed: " + err.Error()}}, nil
		}
	}

	args := []string{"wrangler", "deploy", "--config", data.configPath}
	if env.DryRun {
		args = append(args, "--dry-run")
	}
	env.Logf("$ npx %s", strings.Join(args, " "))
	if err := env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir}, "npx", args...); err != nil {
		return &adapter.Result{Messages: []string{"wrangler deploy failed: " + err.Error()}}, nil
	}
	if env.DryRun {
		return &adapter.Result{OK: true, Messages: []string{"Dry run finished; nothing was deployed."}}, nil
	}
	return &adapter.Result{OK: true, Messages: []string{"Deployed to Cloudflare."}}, nil
}

// Logs streams live Worker logs with `wrangler tail`. Workers keep no log
// history that wrangler can read, so --tail and --since are refused.
func (a *Adapter) Logs(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	if opts.Tail > 0 || opts.Since != "" {
		return errors.New("the cloudflare target only streams live logs (wrangler tail), so --tail and --since are not supported; enable Workers Logs in the Cloudflare dashboard for history")
	}
	if n := len(s.Services); n != 1 {
		return fmt.Errorf("the cloudflare target deploys one Worker, but this spec has %d services", n)
	}
	o, err := decodeOptions(s.Targets["cloudflare"])
	if err != nil {
		return fmt.Errorf("spec.targets.cloudflare: %w", err)
	}

	args := []string{"wrangler", "tail", cmp(o.Name, s.Name), "--format", "pretty"}
	env.Logf("$ npx %s", strings.Join(args, " "))
	if err := env.Exec(ctx, workerExecOptions(o, env), "npx", args...); err != nil {
		return fmt.Errorf("wrangler tail failed (%w); is the Worker deployed and are you logged in (`npx wrangler login`)?", err)
	}
	return nil
}

var _ adapter.Destroyer = (*Adapter)(nil)

func (a *Adapter) DestroySummary(s *spec.Spec, opts adapter.DestroyOptions) ([]string, error) {
	if opts.Volumes {
		return nil, errors.New("--volumes doesn't apply to the cloudflare target, which has no volumes; anyship never deletes D1, KV, R2 or Hyperdrive resources")
	}
	o, err := decodeOptions(s.Targets["cloudflare"])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.cloudflare: %w", err)
	}
	return []string{
		fmt.Sprintf("Delete the Worker %q from Cloudflare.", cmp(o.Name, s.Name)),
		"Keep the D1, KV, R2 and Hyperdrive resources it is bound to.",
	}, nil
}

// Destroy deletes the Worker with `wrangler delete`, which may ask for its
// own confirmation.
func (a *Adapter) Destroy(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	if _, err := a.DestroySummary(s, opts); err != nil {
		return nil, err
	}
	o, err := decodeOptions(s.Targets["cloudflare"])
	if err != nil {
		return nil, fmt.Errorf("spec.targets.cloudflare: %w", err)
	}
	name := cmp(o.Name, s.Name)
	args := []string{"wrangler", "delete", "--name", name}
	env.Logf("$ npx %s", strings.Join(args, " "))
	if err := env.Exec(ctx, workerExecOptions(o, env), "npx", args...); err != nil {
		return &adapter.Result{Messages: []string{"wrangler delete failed: " + err.Error()}}, nil
	}
	return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Deleted the Worker %q; bound resources were kept.", name)}}, nil
}

// workerExecOptions addresses the Worker from the spec alone. anyship keeps no
// state: files left in .anyship/ by an earlier apply may be stale, so they are
// never read back.
func workerExecOptions(o *Options, env *adapter.Env) adapter.ExecOptions {
	opts := adapter.ExecOptions{Dir: env.Dir}
	if o.AccountID != "" {
		opts.Env = []string{"CLOUDFLARE_ACCOUNT_ID=" + o.AccountID}
	}
	return opts
}

func checkService(name string, svc *spec.Service, s *spec.Spec) []adapter.Finding {
	var findings []adapter.Finding
	addError := func(code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: adapter.Error, Code: code, Service: name, Message: message, Hint: hint})
	}

	containerized := svc.Image != "" || svc.Dockerfile != ""
	if containerized {
		addError("CF_CONTAINER_IMAGE",
			"Container images need Cloudflare Containers, which this adapter does not support yet.",
			"Deploy this service to a container/VPS target for now.")
	}
	if svc.Kind == spec.KindWorker {
		addError("CF_BACKGROUND_WORKER",
			"Workers cannot run long-lived background processes.",
			"Move the work to cron triggers or Queues, or use a container/VPS target.")
	}
	if len(svc.Volumes) > 0 {
		var volumes []string
		for _, v := range svc.Volumes {
			volumes = append(volumes, v.Name)
		}
		addError("CF_VOLUMES",
			fmt.Sprintf("Workers have no persistent disk (volumes: %s).", strings.Join(volumes, ", ")),
			"Store data in D1 (sqlite), R2 (bucket) or KV resources instead.")
	}
	for _, port := range svc.Ports {
		if port.Protocol != spec.ProtocolHTTP {
			addError("CF_NON_HTTP_PORT", fmt.Sprintf("Port %d uses %s; Workers only receive HTTP requests.", port.Port, port.Protocol), "")
		}
	}
	if svc.Kind == spec.KindServer && !containerized {
		lang := ""
		if svc.Runtime != nil {
			lang = svc.Runtime.Language
		}
		switch {
		case lang != "" && lang != "javascript" && lang != "typescript":
			addError("CF_LANGUAGE",
				fmt.Sprintf("Workers run JavaScript, TypeScript or WebAssembly, but this is a %s service.", lang),
				"Deploy it to the vps target instead.")
		case svc.Framework() == "nextjs":
			addError("CF_FRAMEWORK_ADAPTER",
				"Next.js needs the OpenNext Cloudflare adapter, which anyship does not drive yet.",
				"Build with @opennextjs/cloudflare and point services.<name>.entry at its worker output.")
		case svc.EdgeIncompatible():
			addError("CF_EDGE_INCOMPATIBLE",
				"This service uses APIs that Cloudflare Workers do not provide (see `anyship init` findings).",
				"Remove the Node-only code paths, or use a container/VPS target.")
		case svc.Entry == "":
			addError("CF_NO_ENTRY",
				"Workers need an entry module that exports a fetch handler.",
				`Set services.<name>.entry, e.g. "src/index.ts".`)
		}
	}
	for _, resourceName := range svc.Uses {
		typ := s.Resources[resourceName].Type
		if bindingRules[typ] == nil {
			addError("CF_UNSUPPORTED_RESOURCE",
				fmt.Sprintf("Cloudflare has no managed %s (resource %q).", typ, resourceName),
				"Use an HTTP-based external service, or KV for simple caching.")
		}
	}
	if refs := svc.RefersTo(); len(refs) > 0 {
		addError("CF_SERVICE_URL", fmt.Sprintf("env refers to ${services.%s.url}, but a Worker's URL depends on the account's workers.dev subdomain or a custom domain, which anyship doesn't know.", refs[0]),
			"Put the URL in env by hand.")
	}
	if svc.CPU != 0 || svc.Memory != "" {
		findings = append(findings, adapter.Finding{Level: adapter.Warning, Code: "CF_RESOURCES_IGNORED", Service: name,
			Message: "memory and cpu aren't applied: Cloudflare gives every Worker the same limits (128 MB of memory), whatever the spec asks for."})
	}
	if svc.Replicas > 1 {
		findings = append(findings, adapter.Finding{Level: adapter.Info, Code: "CF_REPLICAS_IGNORED", Service: name, Message: "Workers scale automatically; replicas is ignored."})
	}
	for _, c := range svc.Cron {
		if c.Command != "" {
			findings = append(findings, adapter.Finding{
				Level: adapter.Warning, Code: "CF_CRON_COMMAND", Service: name,
				Message: "Cron triggers call the Worker's scheduled() handler; cron commands are ignored.",
			})
			break
		}
	}
	for _, c := range svc.Cron {
		if c.Path != "" {
			findings = append(findings, adapter.Finding{
				Level: adapter.Warning, Code: "CF_CRON_PATH", Service: name,
				Message: "Cron triggers call the Worker's scheduled() handler, not a path; the path of a cron entry is ignored.",
				Hint:    "Do the work in scheduled(), or have it call the path's handler.",
			})
			break
		}
	}
	return findings
}

// decodeOptions reads targets.cloudflare strictly, so typos surface as findings.
func decodeOptions(raw json.RawMessage) (*Options, error) {
	opts := &Options{}
	if len(raw) == 0 {
		return opts, nil
	}
	if err := adapter.DecodeOptions(raw, opts); err != nil {
		return nil, err
	}
	if opts.CompatibilityDate != "" && !dateRe.MatchString(opts.CompatibilityDate) {
		return nil, fmt.Errorf("compatibilityDate must look like YYYY-MM-DD, got %q", opts.CompatibilityDate)
	}
	for name, b := range opts.Bindings {
		if b.ID == "" {
			return nil, fmt.Errorf("bindings.%s.id must not be empty", name)
		}
	}
	return opts, nil
}

func render(config wranglerConfig) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Generated by anyship from anyship.yaml. Edit anyship.yaml instead; this file is overwritten.\n")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cmp(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
