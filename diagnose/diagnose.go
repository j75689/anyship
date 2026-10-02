// Package diagnose explains why a deployment fails and proposes a fix to
// anyship.json.
//
// It collects deterministic facts first (plan findings, the target's checks,
// status, logs, generated files), redacts secrets, then asks a model. The
// model only proposes: a spec change is returned only when it applies cleanly
// and the patched spec parses and plans without errors, and nothing is
// written without the user's consent.
package diagnose

import (
	"context"
	"fmt"
)

// Diagnosis is a model's explanation of a failure.
type Diagnosis struct {
	Summary    string    `json:"summary"`
	RootCause  string    `json:"root_cause"`
	Confidence string    `json:"confidence"`
	Evidence   []string  `json:"evidence"`
	Steps      []string  `json:"steps"`
	SpecPatch  []PatchOp `json:"spec_patch"`
	// CodeChange describes changes needed outside anyship.json, if any.
	CodeChange string `json:"code_change"`
}

// Model produces a diagnosis from the collected context.
type Model interface {
	Diagnose(ctx context.Context, req Request) (*Diagnosis, error)
}

type Request struct {
	// Context is the rendered, redacted deployment context.
	Context string
	// Rejected lists earlier proposals that didn't validate, oldest first.
	Rejected []Attempt
}

// Attempt is a proposed spec patch and why it was rejected.
type Attempt struct {
	Patch   []PatchOp
	Problem string
}

// Result is the final diagnosis and, when the model proposed a usable spec
// change, the patched anyship.json.
type Result struct {
	Diagnosis *Diagnosis
	// Patched is the proposed anyship.json; nil without a valid proposal.
	Patched []byte
	// PatchProblem explains why the last proposed change was unusable.
	PatchProblem string
}

// Validator checks a patched anyship.json, for example by parsing and
// planning it for the target.
type Validator func(patched []byte) error

// MaxAttempts bounds how often a rejected spec change is sent back to the model.
const MaxAttempts = 3

// Run asks the model for a diagnosis and validates any spec change it
// proposes, feeding rejections back until a change validates or attempts
// run out.
func Run(ctx context.Context, model Model, contextText string, spec []byte, validate Validator) (*Result, error) {
	req := Request{Context: contextText}
	for attempt := 1; ; attempt++ {
		d, err := model.Diagnose(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(d.SpecPatch) == 0 {
			return &Result{Diagnosis: d}, nil
		}
		patched, err := ApplyPatch(spec, d.SpecPatch)
		if err == nil {
			err = validate(patched)
		}
		if err == nil {
			return &Result{Diagnosis: d, Patched: patched}, nil
		}
		if attempt == MaxAttempts {
			return &Result{Diagnosis: d, PatchProblem: err.Error()}, nil
		}
		req.Rejected = append(req.Rejected, Attempt{Patch: d.SpecPatch, Problem: err.Error()})
	}
}

// ValidationError wraps a reason a patched spec was rejected.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems: %v", len(e.Problems), e.Problems)
}
