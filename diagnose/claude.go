package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/j75689/anyship/spec"
)

const (
	DefaultModel  = "claude-opus-5-5"
	DefaultEffort = "high"
)

// Models that accept the server-side refusal fallback in its "default" form.
var defaultFallbackModels = []string{"claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5"}

// Claude diagnoses with the Claude API. Credentials come from the standard
// sources: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or an `ant auth login` profile.
type Claude struct {
	client anthropic.Client
	model  string
	effort string
}

func NewClaude(model, effort string, opts ...option.RequestOption) *Claude {
	if model == "" {
		model = DefaultModel
	}
	if effort == "" {
		effort = DefaultEffort
	}
	return &Claude{client: anthropic.NewClient(opts...), model: model, effort: effort}
}

func (c *Claude) Diagnose(ctx context.Context, req Request) (*Diagnosis, error) {
	system, err := systemPrompt()
	if err != nil {
		return nil, err
	}
	params := anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: 16000,
		// Stable across runs, so it is cached; the deployment context follows.
		System: []anthropic.BetaTextBlockParam{{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(req))),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(c.effort),
			Format: anthropic.BetaJSONOutputFormatParam{Schema: diagnosisSchema},
		},
	}
	if slices.Contains(defaultFallbackModels, c.model) {
		// If a safety classifier declines, another model answers in the same call.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}

	resp, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return nil, explainAPIError(err)
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, fmt.Errorf("the model declined to answer (%s): %s", resp.StopDetails.Category, resp.StopDetails.Explanation)
	case anthropic.BetaStopReasonMaxTokens:
		return nil, errors.New("the model's answer was cut off; try again with a lower --effort")
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	var d Diagnosis
	if err := json.Unmarshal([]byte(text.String()), &d); err != nil {
		return nil, fmt.Errorf("the model's answer wasn't the expected JSON: %w", err)
	}
	return &d, nil
}

// explainAPIError turns API failures into advice on what to do next.
func explainAPIError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("couldn't reach the Claude API: %w", err)
	}
	switch apiErr.StatusCode {
	case 401, 403:
		return errors.New("the Claude API rejected the credentials; set ANTHROPIC_API_KEY, or run `ant auth login`")
	case 404:
		return fmt.Errorf("the Claude API doesn't know this model; check --model (%w)", err)
	case 429:
		return errors.New("the Claude API is rate limiting this key; wait a moment and try again")
	default:
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("the Claude API had a server error (%d); try again shortly", apiErr.StatusCode)
		}
		return fmt.Errorf("the Claude API refused the request: %w", err)
	}
}

const instructions = `You diagnose failed or unhealthy deployments made with anyship, a CLI that deploys an app described by anyship.yaml to a target platform (vps: Docker Compose over SSH; cloudflare: Workers; gcp: Cloud Run; aws: ECS Express Mode).

You get the user's anyship.yaml, anyship's plan findings, the files anyship generated (compose.yaml, Dockerfiles, wrangler config), the target's dry-run checks, the current status and recent logs. Secrets are redacted.

Find the most likely root cause and ground it in the evidence: quote the log lines, finding codes or config that show it. If the evidence doesn't settle it, say so, lower your confidence, and say what to check next.

Propose a spec_patch only when the fix belongs in anyship.yaml. Paths are JSON Pointers into the anyship.yaml shown, read as JSON (for example /spec/services/web/ports/0/port), and each value is the new value encoded as JSON (a JSON string value needs its quotes, like "\"8080\""; remove takes ""). Never invent secret values. Put changes needed elsewhere (application code, a hand-written Dockerfile, the host) in code_change, and keep steps short and concrete.

The anyship.yaml JSON Schema follows.
`

func systemPrompt() (string, error) {
	schema, err := spec.JSONSchema()
	if err != nil {
		return "", err
	}
	return instructions + "\n" + string(schema), nil
}

func userPrompt(req Request) string {
	var b strings.Builder
	b.WriteString("Diagnose this deployment.\n\n")
	b.WriteString(req.Context)
	for i, a := range req.Rejected {
		patch, _ := json.Marshal(a.Patch)
		fmt.Fprintf(&b, "\nYour proposed spec_patch #%d was rejected and was not applied:\n%s\nReason: %s\n", i+1, patch, a.Problem)
	}
	if len(req.Rejected) > 0 {
		b.WriteString("\nPropose a corrected spec_patch, or an empty one if the fix doesn't belong in anyship.yaml.\n")
	}
	return b.String()
}

var stringList = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

var diagnosisSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"summary", "root_cause", "confidence", "evidence", "steps", "spec_patch", "code_change"},
	"properties": map[string]any{
		"summary":    map[string]any{"type": "string", "description": "One or two sentences on what is wrong."},
		"root_cause": map[string]any{"type": "string", "description": "The most likely root cause, explained."},
		"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		"evidence":   stringList,
		"steps":      stringList,
		"spec_patch": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"op", "path", "value"},
				"properties": map[string]any{
					"op":    map[string]any{"type": "string", "enum": []string{"add", "replace", "remove"}},
					"path":  map[string]any{"type": "string", "description": "JSON Pointer into anyship.yaml"},
					"value": map[string]any{"type": "string", "description": "The new value encoded as JSON; empty for remove"},
				},
			},
		},
		"code_change": map[string]any{"type": "string", "description": "Changes needed outside anyship.yaml, or empty."},
	},
}
