package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/detect"
	"github.com/j75689/anyship/diagnose"
	"github.com/j75689/anyship/spec"
)

const mcpInstructions = `anyship deploys an app described by anyship.json to a target platform.

Typical flow: detect (draft a spec for a directory) → write anyship.json → validate → plan → apply with dry_run → apply.
Read-only tools never change anything. Deploying and destroying are only possible when the server was started with --allow-deploy.
Never pass volumes=true to destroy unless the user explicitly asked to delete data; it deletes volumes and secrets for good.`

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
	Config string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
}

type targetInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
	Target string `json:"target" jsonschema:"deploy target such as vps or cloudflare; the targets tool lists them"`
}

type targetsOutput struct {
	Targets []targetInfo `json:"targets"`
}

type targetInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
}

type detectInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"project directory; defaults to the server's working directory"`
}

type detectOutput struct {
	// Spec is the drafted anyship.json, to review and write to the project.
	Spec     any               `json:"spec"`
	Valid    bool              `json:"valid"`
	Problems []string          `json:"problems"`
	Findings []adapter.Finding `json:"findings"`
	Evidence []string          `json:"evidence"`
}

type validateOutput struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
	Name     string   `json:"name,omitempty"`
	Services []string `json:"services"`
}

type planOutput struct {
	Target   string            `json:"target"`
	Ready    bool              `json:"ready"`
	Findings []adapter.Finding `json:"findings"`
	Actions  []adapter.Action  `json:"actions"`
	Files    []string          `json:"files"`
}

type logsInput struct {
	Config  string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
	Target  string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	Service string `json:"service,omitempty" jsonschema:"only this service's logs"`
	Tail    int    `json:"tail,omitempty" jsonschema:"recent lines per service; the target's default when 0"`
	Since   string `json:"since,omitempty" jsonschema:"only logs newer than a duration such as 10m or an RFC 3339 timestamp"`
}

type logsOutput struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	// Note explains a partial read, such as a live stream cut off after a while.
	Note string `json:"note,omitempty"`
}

type diagnoseContextInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
	Target string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	Note   string `json:"note,omitempty" jsonschema:"what the user saw, such as an error message"`
}

type diagnoseContextOutput struct {
	// Context is Markdown with one section per source.
	Context string `json:"context"`
}

type applyInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
	Target string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"only run the target's checks; change nothing"`
}

type destroyInput struct {
	Config  string `json:"config,omitempty" jsonschema:"path to anyship.json; defaults to anyship.json in the server's working directory"`
	Target  string `json:"target" jsonschema:"deploy target such as vps or cloudflare"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"only describe what would be removed"`
	Volumes bool   `json:"volumes,omitempty" jsonschema:"also delete volumes, secrets and deployment files; only when the user explicitly asked"`
	// ConfirmProject guards data deletion, like typing the name in the CLI.
	ConfirmProject string `json:"confirm_project,omitempty" jsonschema:"required with volumes: the spec's name, as the user confirmed it"`
}

type resultOutput struct {
	OK       bool              `json:"ok"`
	Summary  []string          `json:"summary,omitempty"`
	Findings []adapter.Finding `json:"findings"`
	Messages []string          `json:"messages"`
	// Output is what the target's tools (ssh, wrangler, ...) printed.
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func newMCPServer(a *app, version string, allowDeploy bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "anyship", Version: version}, &mcp.ServerOptions{Instructions: mcpInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	readsTarget := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(server, &mcp.Tool{Name: "targets", Description: "List deploy targets and what each supports.", Annotations: readOnly},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, targetsOutput, error) {
			out := targetsOutput{Targets: []targetInfo{}}
			for _, ad := range a.registry.List() {
				out.Targets = append(out.Targets, targetInfo{ad.Name(), ad.Description(), capabilities(ad)})
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "detect",
		Description: "Detect a project's stack and draft an anyship.json for it. Nothing is written; review the draft, then save it as anyship.json.",
		Annotations: readOnly,
	}, func(_ context.Context, _ *mcp.CallToolRequest, in detectInput) (*mcp.CallToolResult, detectOutput, error) {
		d, err := detect.Project(orDefault(in.Dir, "."))
		if err != nil {
			return nil, detectOutput{}, err
		}
		data, err := json.Marshal(d.Spec)
		if err != nil {
			return nil, detectOutput{}, err
		}
		out := detectOutput{Valid: true, Problems: []string{}, Findings: nonNil(d.Findings), Evidence: nonNil(d.Evidence)}
		if err := json.Unmarshal(data, &out.Spec); err != nil {
			return nil, detectOutput{}, err
		}
		if _, err := spec.Parse(data); err != nil {
			out.Valid, out.Problems = false, problemsOrError(err)
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "validate", Description: "Check anyship.json against the schema and cross-references.", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in configInput) (*mcp.CallToolResult, validateOutput, error) {
			s, err := spec.Load(orDefault(in.Config, spec.Filename))
			if err != nil {
				return nil, validateOutput{Problems: problemsOrError(err), Services: []string{}}, nil
			}
			return nil, validateOutput{Valid: true, Problems: []string{}, Name: s.Name, Services: s.ServiceNames()}, nil
		})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "plan",
		Description: "Show what deploying to a target would do, and every need the target can't meet. Changes nothing.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in targetInput) (*mcp.CallToolResult, planOutput, error) {
		d, _, err := a.prepareForMCP(in.Config, in.Target, false)
		if err != nil {
			return nil, planOutput{}, err
		}
		p, err := d.adapter.Plan(ctx, d.spec, d.env)
		if err != nil {
			return nil, planOutput{}, err
		}
		out := planOutput{Target: p.Target, Ready: !adapter.HasErrors(p.Findings), Findings: nonNil(p.Findings), Actions: nonNil(p.Actions), Files: []string{}}
		for _, f := range p.Files {
			out.Files = append(out.Files, f.Path)
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "status", Description: "Show what is running for the spec on a target.", Annotations: readsTarget},
		func(ctx context.Context, _ *mcp.CallToolRequest, in targetInput) (*mcp.CallToolResult, *adapter.Status, error) {
			d, _, err := a.prepareForMCP(in.Config, in.Target, false)
			if err != nil {
				return nil, nil, err
			}
			reader, ok := d.adapter.(adapter.StatusReader)
			if !ok {
				return nil, nil, fmt.Errorf("the %s target does not report status yet", d.adapter.Name())
			}
			st, err := reader.Status(ctx, d.spec, d.env)
			return nil, st, err
		})

	mcp.AddTool(server, &mcp.Tool{
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
		err = reader.Logs(window, d.spec, d.env, adapter.LogOptions{Service: in.Service, Tail: in.Tail, Since: in.Since})
		out := logsOutput{}
		out.Output, out.Truncated = output.String()
		if window.Err() != nil && ctx.Err() == nil {
			out.Note = fmt.Sprintf("stopped reading after %s; this target streams live logs", liveLogWindow)
			err = nil
		}
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "diagnose_context",
		Description: "Collect what anyship knows about the spec on a target, with secrets redacted: plan findings, the target's dry-run checks, " +
			"status, recent logs and generated files. Use it to work out why a deployment fails; propose fixes to anyship.json from it.",
		Annotations: readsTarget,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in diagnoseContextInput) (*mcp.CallToolResult, diagnoseContextOutput, error) {
		config := orDefault(in.Config, spec.Filename)
		d, _, err := a.prepareForMCP(config, in.Target, false)
		if err != nil {
			return nil, diagnoseContextOutput{}, err
		}
		raw, err := os.ReadFile(config)
		if err != nil {
			return nil, diagnoseContextOutput{}, err
		}
		collected := diagnose.Collect(ctx, diagnose.Inputs{
			Spec: d.spec, SpecPath: config, SpecRaw: raw, Adapter: d.adapter,
			NewEnv: capturingEnv(d.env), Note: in.Note, Checks: true,
			Redactor: diagnose.NewRedactor(d.spec, os.LookupEnv),
		})
		return nil, diagnoseContextOutput{Context: collected.Render()}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "apply",
		Description: "Deploy the spec to a target. With dry_run, only run the target's checks. " +
			"Real deploys need the server to run with --allow-deploy; ask the user before deploying.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applyInput) (*mcp.CallToolResult, resultOutput, error) {
		if !in.DryRun && !allowDeploy {
			return nil, resultOutput{}, errors.New("this anyship MCP server only allows dry runs; restart it with `anyship mcp --allow-deploy` to deploy")
		}
		d, output, err := a.prepareForMCP(in.Config, in.Target, in.DryRun)
		if err != nil {
			return nil, resultOutput{}, err
		}
		p, err := d.adapter.Plan(ctx, d.spec, d.env)
		if err != nil {
			return nil, resultOutput{}, err
		}
		if adapter.HasErrors(p.Findings) {
			return nil, resultOutput{Findings: p.Findings, Messages: []string{"The plan has errors; nothing was done. Fix anyship.json and plan again."}}, nil
		}
		result, err := d.adapter.Apply(ctx, p, d.spec, d.env)
		if err != nil {
			return nil, resultOutput{}, err
		}
		return nil, toResultOutput(result, nil, output), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "destroy",
		Description: "Remove the spec's deployment from a target, keeping persistent data unless volumes is set. " +
			"Real removals need the server to run with --allow-deploy; always ask the user first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
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
			return nil, resultOutput{OK: true, Summary: summary, Findings: []adapter.Finding{}, Messages: []string{"Dry run: nothing was removed."}}, nil
		}
		if in.Volumes && in.ConfirmProject != d.spec.Name {
			return nil, resultOutput{}, fmt.Errorf("deleting data needs confirm_project set to %q, confirmed with the user", d.spec.Name)
		}
		result, err := destroyer.Destroy(ctx, d.spec, d.env, opts)
		if err != nil {
			return nil, resultOutput{}, err
		}
		return nil, toResultOutput(result, summary, output), nil
	})

	return server
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
		return nil, nil, err
	}
	output := &tailBuffer{max: maxToolOutput}
	d.env.Logf = func(format string, args ...any) { fmt.Fprintf(output, format+"\n", args...) }
	d.env.Exec = func(ctx context.Context, opts adapter.ExecOptions, name string, args ...string) error {
		return runWith(ctx, opts, stdio{in: bytes.NewReader(nil), out: output, err: output}, name, args...)
	}
	return d, output, nil
}

func toResultOutput(r *adapter.Result, summary []string, output *tailBuffer) resultOutput {
	out := resultOutput{OK: r.OK, Summary: summary, Findings: nonNil(r.Findings), Messages: nonNil(r.Messages)}
	out.Output, out.Truncated = output.String()
	return out
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

func problemsOrError(err error) []string {
	if problems := problemsOf(err); len(problems) > 0 {
		return problems
	}
	return []string{err.Error()}
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

// nonNil keeps empty lists as [] in JSON output.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func ptr[T any](v T) *T { return &v }
