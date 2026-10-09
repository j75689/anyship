package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/j75689/anyship"
	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

const mcpInstructions = `anyship deploys an app described by anyship.yaml to a target platform.

Typical flow for a directory without anyship.yaml: detect (draft a spec) → write anyship.yaml → validate → plan → apply with dry_run → apply.
If anyship.yaml already exists, start from it: validate → plan → apply; detect then says so (existing) and its draft is only for comparison, not a replacement.
When a target's CLI is missing or not logged in, doctor says which one and how to fix it.
Read-only tools never change anything. Deploying and destroying are only possible when the server was started with --allow-deploy.
Never pass volumes=true to destroy unless the user explicitly asked to delete data; it deletes volumes and secrets for good.
Resources: anyship://schema is the JSON schema of anyship.yaml, and anyship://targets/<name> is each target's page with its options under spec.targets.<name>, how it deploys and what it refuses. Read a target's page before writing its options.`

// mcpInstructionsIn adds where the server runs. An agent's own working
// directory need not be the server's, and every relative path a tool takes
// is resolved from here.
func mcpInstructionsIn(dir string) string {
	return mcpInstructions + fmt.Sprintf(`

This server runs in %s. A relative config or dir is resolved from there, and config defaults to anyship.yaml in it.
In a repository with several apps, pass the app's directory as dir to detect and the path of its anyship.yaml as config.`, dir)
}

// forAgent rewords an error for a caller that has tools, not a shell.
func forAgent(err error) error {
	if err == nil {
		return nil
	}
	err = causeForUser(err)
	var missing *spec.NotFoundError
	if errors.As(err, &missing) {
		return fmt.Errorf("%s does not exist; draft a spec with the detect tool and write it there, or pass config if the spec is somewhere else", missing.Path)
	}
	if text := agentText(err.Error()); text != err.Error() {
		return errors.New(text)
	}
	return err
}

// Messages are written once, in the adapters and in detection, for someone at
// a command line: "has it been deployed with `anyship apply -t vps`?". An
// agent that reached anyship over MCP has tools and maybe no shell, so the
// commands a message names become the tools that do the same thing. Advice
// about other programs (ssh, gcloud, restarting this server) is left as it
// is, for the agent to pass on.
var (
	backtickedCommand = regexp.MustCompile("`anyship (init|apply|status|logs|destroy|plan|validate|diagnose|doctor)\\b([^`]*)`")
	bareCommand       = regexp.MustCompile(`\banyship (init|apply|status|logs|destroy|plan|validate|diagnose|doctor)\b`)
	bareFlags         = strings.NewReplacer("destroy --volumes", "destroy with volumes=true", "apply --dry-run", "apply with dry_run=true")
	// targetFlag is a command line whose only flag picks the target, which
	// becomes the tool's target argument.
	targetFlag = regexp.MustCompile(`^ -t ([a-z]+)$`)
)

// toolFor names the tool behind a command, where the two differ.
var toolFor = map[string]string{"init": "detect", "diagnose": "diagnose_context"}

// targetsPage matches the command that prints a target's page, which option
// hints end with; an agent reads the page as a resource instead.
var targetsPage = regexp.MustCompile("`anyship targets ([a-z]+)`")

// Resource URIs. The schema is made by the binary, so it is always the
// schema of the spec this version reads; the target pages travel with it.
const (
	schemaURI     = "anyship://schema"
	targetURIBase = "anyship://targets/"
)

func targetURI(name string) string { return targetURIBase + name }

func agentText(text string) string {
	tool := func(command string) string {
		if name, ok := toolFor[command]; ok {
			command = name
		}
		return "the " + command + " tool"
	}
	text = backtickedCommand.ReplaceAllStringFunc(text, func(match string) string {
		m := backtickedCommand.FindStringSubmatch(match)
		switch {
		case targetFlag.MatchString(m[2]):
			return tool(m[1]) + " with target=" + targetFlag.FindStringSubmatch(m[2])[1]
		case strings.Contains(m[2], "--volumes"):
			return tool(m[1]) + " with volumes=true"
		case strings.Contains(m[2], "--dry-run"):
			return tool(m[1]) + " with dry_run=true"
		case strings.Contains(m[2], "--image"):
			return tool(m[1]) + " with its images argument"
		}
		return tool(m[1])
	})
	text = bareCommand.ReplaceAllStringFunc(text, func(match string) string {
		return tool(bareCommand.FindStringSubmatch(match)[1])
	})
	text = targetsPage.ReplaceAllStringFunc(text, func(match string) string {
		return "the " + targetURI(targetsPage.FindStringSubmatch(match)[1]) + " resource"
	})
	return bareFlags.Replace(text)
}

// addTool registers a tool whose errors are worded for an agent.
func addTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)) {
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		result, out, err := handler(ctx, req, in)
		return result, out, forAgent(err)
	})
}

// maxToolOutput caps the command output returned to the client, keeping the end.
const maxToolOutput = 32 << 10

// liveLogWindow bounds log reads on targets that only stream live logs.
const liveLogWindow = 20 * time.Second

func (a *app) mcpCommand(version string) *cobra.Command {
	var allowDeploy bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve anyship to AI agents over the Model Context Protocol (stdio)",
		Long: `Serve anyship to AI agents over the Model Context Protocol on stdin/stdout.

Add it to Claude Code with:  claude mcp add anyship -- anyship mcp

Read-only tools and dry runs are always available. Deploying and destroying
also need --allow-deploy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return newMCPServer(a, version, allowDeploy).Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	cmd.Flags().BoolVar(&allowDeploy, "allow-deploy", false, "let agents deploy and destroy, not only plan and dry-run")
	return cmd
}

type configInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
}

type targetInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target string `json:"target" jsonschema:"deploy target such as vps or cloudflare; the targets tool lists them"`
}

type planInput struct {
	Config string            `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target string            `json:"target" jsonschema:"deploy target such as vps or cloudflare; the targets tool lists them"`
	Images map[string]string `json:"images,omitempty" jsonschema:"image to deploy per service instead of the one in the spec, for this call only"`
}

type detectInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"project directory; defaults to the server's working directory"`
}

type logsInput struct {
	Config     string `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target     string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	Service    string `json:"service,omitempty" jsonschema:"only this service's logs"`
	Tail       int    `json:"tail,omitempty" jsonschema:"recent lines per service; the target's default when 0"`
	Since      string `json:"since,omitempty" jsonschema:"only logs newer than a duration such as 10m or an RFC 3339 timestamp"`
	Timestamps bool   `json:"timestamps,omitempty" jsonschema:"prefix each line with its time"`
}

type logsOutput struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	// Note explains a partial read, such as a live stream cut off after a while.
	Note string `json:"note,omitempty"`
}

type diagnoseContextInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	Note   string `json:"note,omitempty" jsonschema:"what the user saw, such as an error message"`
}

type diagnoseContextOutput struct {
	// Context is Markdown with one section per source.
	Context string `json:"context"`
}

type applyInput struct {
	Config string            `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target string            `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	DryRun bool              `json:"dry_run,omitempty" jsonschema:"only run the target's checks and write the generated files under .anyship/; deploy nothing"`
	Images map[string]string `json:"images,omitempty" jsonschema:"image to deploy per service instead of the one in the spec, for this call only"`
}

type destroyInput struct {
	Config  string `json:"config,omitempty" jsonschema:"path to anyship.yaml; defaults to anyship.yaml in the server's working directory"`
	Target  string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"only describe what would be removed"`
	Volumes bool   `json:"volumes,omitempty" jsonschema:"also delete volumes, secrets and deployment files; only when the user explicitly asked"`
	// ConfirmProject guards data deletion, like typing the name in the CLI.
	ConfirmProject string `json:"confirm_project,omitempty" jsonschema:"required with volumes: the spec's name, as the user confirmed it"`
}

func newMCPServer(a *app, version string, allowDeploy bool) *mcp.Server {
	instructions := mcpInstructions
	if dir, err := os.Getwd(); err == nil {
		instructions = mcpInstructionsIn(dir)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "anyship", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	// Reading twice changes nothing more than reading once. The SDK sends
	// idempotentHint either way, so leaving it unset would claim the opposite.
	// Every tool states all four hints. A host reads an absent hint as the
	// protocol's default, which is the wrong one here (destructive, open
	// world), and some tool directories reject a tool with a hint missing.
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}
	// readsTarget tools ask the platform, so they reach outside the host.
	readsTarget := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(true)}
	deploys := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: false, OpenWorldHint: ptr(true)}
	removes := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)}

	addResources(server, a.registry)

	addTool(server, &mcp.Tool{Name: "targets", Description: "List deploy targets, what each supports, and the resource with each one's page (its options, how it deploys, what it refuses).", Annotations: readOnly},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, targetsOutput, error) {
			return nil, newTargetsOutput(a.registry, targetURI), nil
		})

	addTool(server, &mcp.Tool{
		Name: "detect",
		Description: "Detect a project's stack and draft an anyship.yaml for it. Nothing is written; review the draft, then save it as anyship.yaml. " +
			"If the directory already has an anyship.yaml, existing says so with its validation and targets: validate and plan that one instead of replacing it.",
		Annotations: readOnly,
	}, func(_ context.Context, _ *mcp.CallToolRequest, in detectInput) (*mcp.CallToolResult, detectOutput, error) {
		out, err := newDetectOutput(orDefault(in.Dir, "."), agent)
		return nil, out, err
	})

	addTool(server, &mcp.Tool{Name: "validate", Description: "Check anyship.yaml against the schema and cross-references.", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in configInput) (*mcp.CallToolResult, validateOutput, error) {
			return nil, newValidateOutput(orDefault(in.Config, spec.Filename), agent), nil
		})

	addTool(server, &mcp.Tool{
		Name: "plan",
		Description: "Show what deploying to a target would do, and every need the target can't meet. Changes nothing: " +
			"the files the target would generate come back with their contents, so review them here instead of reading their paths.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in planInput) (*mcp.CallToolResult, planOutput, error) {
		d, _, err := a.prepareForMCP(in.Config, in.Target, false)
		if err != nil {
			return nil, planOutput{}, err
		}
		if err := overrideImages(d.spec, in.Images); err != nil {
			return nil, planOutput{}, err
		}
		p, err := d.adapter.Plan(ctx, d.spec, d.env)
		if err != nil {
			return nil, planOutput{}, err
		}
		return nil, newPlanOutput(p, agent), nil
	})

	addTool(server, &mcp.Tool{Name: "status", Description: "Show what is running for the spec on a target; healthy says whether everything runs as the spec asks.", Annotations: readsTarget},
		func(ctx context.Context, _ *mcp.CallToolRequest, in targetInput) (*mcp.CallToolResult, statusOutput, error) {
			d, _, err := a.prepareForMCP(in.Config, in.Target, false)
			if err != nil {
				return nil, statusOutput{}, err
			}
			reader, ok := d.adapter.(adapter.StatusReader)
			if !ok {
				return nil, statusOutput{}, fmt.Errorf("the %s target does not report status yet", d.adapter.Name())
			}
			st, err := reader.Status(ctx, d.spec, d.env)
			if err != nil {
				return nil, statusOutput{}, err
			}
			return nil, newStatusOutput(st), nil
		})

	addTool(server, &mcp.Tool{
		Name: "doctor",
		Description: "Check that this machine has the command-line tools a target deploys with (gcloud, aws, kubectl, docker buildx, node and wrangler, ssh), " +
			"their versions and their logins. Run it first when a target's tool fails or a check says it isn't installed or logged in. Deploys and changes nothing.",
		Annotations: readsTarget,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in doctorInput) (*mcp.CallToolResult, doctorOutput, error) {
		out, err := a.newDoctorOutput(ctx, orDefault(in.Config, spec.Filename), in.Config != "", in.Target, agent)
		return nil, out, err
	})

	addTool(server, &mcp.Tool{
		Name:        "logs",
		Description: fmt.Sprintf("Read recent runtime logs from a target. Targets that only stream live logs are read for %s.", liveLogWindow),
		Annotations: readsTarget,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
		d, output, err := a.prepareForMCP(in.Config, in.Target, false)
		if err != nil {
			return nil, logsOutput{}, err
		}
		reader, ok := d.adapter.(adapter.LogReader)
		if !ok {
			return nil, logsOutput{}, fmt.Errorf("the %s target does not provide logs", d.adapter.Name())
		}
		if in.Service != "" {
			if _, ok := d.spec.Services[in.Service]; !ok {
				return nil, logsOutput{}, fmt.Errorf("unknown service %q; services: %s", in.Service, strings.Join(d.spec.ServiceNames(), ", "))
			}
		}
		if in.Since != "" && !validSince(in.Since) {
			return nil, logsOutput{}, fmt.Errorf("since %q is not a duration like 10m or an RFC 3339 timestamp", in.Since)
		}
		window, cancel := context.WithTimeout(ctx, liveLogWindow)
		defer cancel()
		err = reader.Logs(window, d.spec, d.env, adapter.LogOptions{Service: in.Service, Tail: in.Tail, Since: in.Since, Timestamps: in.Timestamps})
		out := logsOutput{}
		out.Output, out.Truncated = output.String()
		if window.Err() != nil && ctx.Err() == nil {
			out.Note = fmt.Sprintf("stopped reading after %s; this target streams live logs", liveLogWindow)
			err = nil
		}
		return nil, out, err
	})

	addTool(server, &mcp.Tool{
		Name: "diagnose_context",
		Description: "Collect what anyship knows about the spec on a target, with secrets redacted: plan findings, the target's dry-run checks, " +
			"status, recent logs and generated files. Use it to work out why a deployment fails; propose fixes to anyship.yaml from it.",
		Annotations: readsTarget,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in diagnoseContextInput) (*mcp.CallToolResult, diagnoseContextOutput, error) {
		config := orDefault(in.Config, spec.Filename)
		d, _, err := a.prepareForMCP(config, in.Target, false)
		if err != nil {
			return nil, diagnoseContextOutput{}, err
		}
		text, err := diagnoseContext(ctx, d, config, in.Note, true, nil)
		return nil, diagnoseContextOutput{Context: text}, err
	})

	addTool(server, &mcp.Tool{
		Name: "apply",
		Description: "Deploy the spec to a target. With dry_run, only run the target's checks and write the generated files under .anyship/. " +
			"Real deploys need the server to run with --allow-deploy; ask the user before deploying.",
		Annotations: deploys,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applyInput) (*mcp.CallToolResult, resultOutput, error) {
		if !in.DryRun && !allowDeploy {
			return nil, resultOutput{}, errors.New("this anyship MCP server only allows dry runs; restart it with `anyship mcp --allow-deploy` to deploy")
		}
		d, output, err := a.prepareForMCP(in.Config, in.Target, in.DryRun)
		if err != nil {
			return nil, resultOutput{}, err
		}
		if err := overrideImages(d.spec, in.Images); err != nil {
			return nil, resultOutput{}, err
		}
		p, err := d.adapter.Plan(ctx, d.spec, d.env)
		if err != nil {
			return nil, resultOutput{}, err
		}
		if adapter.HasErrors(p.Findings) {
			return nil, planErrorsOutput(p, agent), nil
		}
		result, err := d.adapter.Apply(ctx, p, d.spec, d.env)
		if err != nil {
			return nil, resultOutput{}, err
		}
		out := newResultOutput(result, nil, output, agent)
		for _, f := range p.Files {
			out.Files = append(out.Files, f.Path)
		}
		return nil, out, nil
	})

	addTool(server, &mcp.Tool{
		Name: "destroy",
		Description: "Remove the spec's deployment from a target, keeping persistent data unless volumes is set. " +
			"Real removals need the server to run with --allow-deploy; always ask the user first.",
		Annotations: removes,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, resultOutput, error) {
		if !in.DryRun && !allowDeploy {
			return nil, resultOutput{}, errors.New("this anyship MCP server only allows dry runs; restart it with `anyship mcp --allow-deploy` to destroy")
		}
		d, output, err := a.prepareForMCP(in.Config, in.Target, false)
		if err != nil {
			return nil, resultOutput{}, err
		}
		destroyer, ok := d.adapter.(adapter.Destroyer)
		if !ok {
			return nil, resultOutput{}, fmt.Errorf("the %s target can't destroy deployments yet", d.adapter.Name())
		}
		opts := adapter.DestroyOptions{Volumes: in.Volumes}
		summary, err := destroyer.DestroySummary(d.spec, opts)
		if err != nil {
			return nil, resultOutput{}, err
		}
		if in.DryRun {
			return nil, dryRunDestroyOutput(summary, agent), nil
		}
		if in.Volumes && in.ConfirmProject != d.spec.Name {
			return nil, resultOutput{}, fmt.Errorf("deleting data needs confirm_project set to %q, confirmed with the user", d.spec.Name)
		}
		result, err := destroyer.Destroy(ctx, d.spec, d.env, opts)
		if err != nil {
			return nil, resultOutput{}, err
		}
		return nil, newResultOutput(result, summary, output, agent), nil
	})

	return server
}

// targetDoc is the page of a target, as carried by the binary.
func targetDoc(name string) ([]byte, error) {
	return anyship.TargetDocs.ReadFile("docs/targets/" + name + ".md")
}

// addResources serves the spec's JSON schema and each target's page, so an
// agent writing anyship.yaml can read what a hint points at.
func addResources(server *mcp.Server, registry *adapter.Registry) {
	server.AddResource(&mcp.Resource{
		URI: schemaURI, Name: "anyship.yaml schema", MIMEType: "application/schema+json",
		Description: "JSON schema of anyship.yaml: every field, with the targets block left open; a target's own options are on its page.",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		data, err := spec.JSONSchema()
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: schemaURI, MIMEType: "application/schema+json", Text: string(data)}}}, nil
	})
	for _, ad := range registry.List() {
		name := ad.Name()
		page, err := targetDoc(name)
		if err != nil {
			continue
		}
		uri := targetURI(name)
		server.AddResource(&mcp.Resource{
			URI: uri, Name: "the " + name + " target", MIMEType: "text/markdown", Size: int64(len(page)),
			Description: fmt.Sprintf("The %s target: its options under spec.targets.%s, how it deploys a spec, what it refuses and why.", name, name),
		}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: string(page)}}}, nil
		})
	}
}

// prepareForMCP loads the spec and target like the CLI does, but routes all
// progress and command output into a buffer: stdin and stdout carry the MCP
// protocol, so nothing else may read or write them.
func (a *app) prepareForMCP(config, target string, dryRun bool) (*deployment, *tailBuffer, error) {
	if target == "" {
		return nil, nil, fmt.Errorf("target is required; available: %s", strings.Join(a.registry.Names(), ", "))
	}
	d, err := a.prepare(orDefault(config, spec.Filename), target, dryRun)
	if err != nil {
		return nil, nil, forAgent(err)
	}
	output := &tailBuffer{max: maxToolOutput}
	d.env.Logf = func(format string, args ...any) { fmt.Fprintf(output, format+"\n", args...) }
	d.env.Exec = execFor(target, stdio{in: bytes.NewReader(nil), out: output, err: output})
	return d, output, nil
}

// tailBuffer keeps the last max bytes written to it. It is safe for the
// concurrent writes of a command's stdout and stderr.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.truncated = true
	}
	return len(p), nil
}

// String returns the kept output and whether earlier output was dropped.
func (b *tailBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.truncated
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if !filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return value
}

func ptr[T any](v T) *T { return &v }

// withoutLinesAbout drops the lines of text that mention name.
func withoutLinesAbout(text, name string) string {
	lines := strings.Split(text, "\n")
	return strings.Join(slices.DeleteFunc(lines, func(line string) bool { return strings.Contains(line, name) }), "\n")
}
