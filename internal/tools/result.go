// Package tools — image-bearing tool results (computer-use M1).
//
// Most tools return text only, and the Tool interface stays that way:
// Execute returns (string, error) and the 24 pre-existing tools keep
// working unchanged. A tool that needs to return images (screenshot)
// implements ImageResultTool instead and returns a ToolResult — text
// plus up to MaxImagesPerResult images, each at most
// MaxImageBytes. Registry.ExecuteResult is the dispatch point: it
// calls ExecuteResult when the tool implements it and wraps a legacy
// Execute result otherwise, so callers that want images use one
// method and callers that only want text keep using Execute (which
// delegates to ExecuteResult and returns the text).
//
// The caps are enforced twice: the producing tool must keep its
// images inside them (screenshot shrinks its capture until it fits),
// and the registry rejects an over-cap result outright — a tool bug
// must fail loudly, never silently drop an image the model was
// told to expect. Text goes through the same redaction + truncation
// as every other tool result; image bytes are never redacted (they
// are binary) and never truncated (a half-image is worse than an
// error).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
)

// MaxImageBytes is the largest single image a tool result may
// carry: 2 MiB (plan §4.1/§4.7 — one screenshot at 1280px JPEG is
// typically 100–300 KiB, so this leaves ample headroom while
// bounding what one result can put in the provider request).
const MaxImageBytes = 2 << 20

// MaxImagesPerResult is the most images one tool result may carry:
// 4 (plan §4.7). The screenshot tool returns exactly one; the cap
// exists so a future multi-image tool cannot flood a request.
const MaxImagesPerResult = 4

// ToolImage is one image in a tool result: raw encoded bytes
// (JPEG or PNG) plus the MIME type the provider needs to render
// it. It is an alias for model.ImageContent, so a tool result's
// images flow into the agent loop's tool message with no conversion.
type ToolImage = model.ImageContent

// ToolResult is what an image-bearing tool returns: the JSON/text
// summary the model reads, plus the images it can see. A nil or
// empty Images means "text only" — exactly what a legacy tool
// returns.
type ToolResult struct {
	Text   string
	Images []ToolImage
}

// TextOnly builds a text-only result (the legacy shape, for tools
// that implement ExecuteResult without images).
func TextOnly(text string) ToolResult { return ToolResult{Text: text} }

// Validate reports whether the result is inside the image caps: at
// most MaxImagesPerResult images, each at most MaxImageBytes, each
// with a JPEG/PNG media type and non-empty data. Text is not
// validated here — the registry redacts and truncates it like any
// other result.
func (r ToolResult) Validate() error {
	if len(r.Images) > MaxImagesPerResult {
		return fmt.Errorf("tool result carries %d images, max %d", len(r.Images), MaxImagesPerResult)
	}
	for i, img := range r.Images {
		if len(img.Data) == 0 {
			return fmt.Errorf("tool result image %d is empty", i)
		}
		if len(img.Data) > MaxImageBytes {
			return fmt.Errorf("tool result image %d is %d bytes, max %d", i, len(img.Data), MaxImageBytes)
		}
		if img.MIMEType != "image/jpeg" && img.MIMEType != "image/png" {
			return fmt.Errorf("tool result image %d has unsupported media type %q", i, img.MIMEType)
		}
	}
	return nil
}

// ImageResultTool is a tool that returns images with its text. It
// embeds Tool (Name/Description/Definition/Execute stay, so the
// tool still registers and validates like any other) and adds
// ExecuteResult, which the registry prefers over Execute when
// present. A tool implements BOTH: Execute must keep working and
// return the same text ExecuteResult returns, so text-only callers
// (and the journal replay path, which stores text) never see a
// different result than image callers do.
type ImageResultTool interface {
	Tool
	ExecuteResult(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// TurnResetter is a tool that keeps per-turn state (the screenshot
// rate limit's per-turn counter) and needs to know when a new turn
// starts. The agent loop calls Registry.BeginTurn once per turn;
// a tool that does not implement this is unaffected.
type TurnResetter interface {
	BeginTurn()
}

// BeginTurn tells every TurnResetter tool a new turn has started,
// so per-turn counters reset. It is safe on a nil registry and
// never fails: a tool's BeginTurn must not panic (a panicking
// reset is a programming error the registry cannot fix, so it is
// allowed to propagate like any other programming error).
func (r *Registry) BeginTurn() {
	if r == nil {
		return
	}
	for _, t := range r.List() {
		if tr, ok := t.(TurnResetter); ok {
			tr.BeginTurn()
		}
	}
}

// ExecuteResult runs a named tool and returns its full result,
// text plus images. It is Execute with the images preserved: the
// same unknown-tool error, the same central schema validation, the
// same panic recovery, and the same redaction + truncation of the
// text. A tool that implements ImageResultTool is dispatched to
// ExecuteResult; every other tool runs through Execute and its
// text is wrapped as a text-only result. An over-cap image result
// is an execution error, never a silent drop (see ToolResult).
func (r *Registry) ExecuteResult(ctx context.Context, name string, args json.RawMessage) (result ToolResult, err error) {
	// A panicking tool must not crash the daemon (or the agent run):
	// recover here and report the panic as an execution error, the
	// same way Execute does for text results.
	defer func() {
		if rec := recover(); rec != nil {
			result = ToolResult{}
			err = &ToolError{
				Tool: name,
				Kind: "execution",
				Err:  errors.New(security.RedactSecrets(fmt.Sprintf("tool panicked: %v", rec))),
			}
		}
	}()
	t, ok := r.Get(name)
	if !ok {
		return ToolResult{}, &ToolError{Tool: name, Kind: "unknown_tool", Err: fmt.Errorf("unknown tool")}
	}
	// Central schema validation, exactly as in Execute: a call
	// that fails validation never reaches tool code.
	if err := validateArgs(t.Definition(), args); err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			return ToolResult{}, &ToolError{
				Tool: toolErr.Tool,
				Kind: toolErr.Kind,
				Err:  errors.New(TruncateToolResult(security.RedactSecrets(toolErr.Err.Error()), r.MaxResultBytes())),
			}
		}
		return ToolResult{}, err
	}
	var res ToolResult
	if irt, ok := t.(ImageResultTool); ok {
		res, err = irt.ExecuteResult(ctx, args)
	} else {
		var text string
		text, err = t.Execute(ctx, args)
		res = ToolResult{Text: text}
	}
	if err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			return ToolResult{}, &ToolError{
				Tool: toolErr.Tool,
				Kind: toolErr.Kind,
				Err:  errors.New(TruncateToolResult(security.RedactSecrets(toolErr.Err.Error()), r.MaxResultBytes())),
			}
		}
		return ToolResult{}, fmt.Errorf("%s", TruncateToolResult(security.RedactSecrets(err.Error()), r.MaxResultBytes()))
	}
	if err := res.Validate(); err != nil {
		return ToolResult{}, &ToolError{Tool: name, Kind: "execution", Err: err}
	}
	// Central choke point for tool-result text, exactly as in
	// Execute: every tool's output passes through here. Images
	// pass through unmodified — they were validated above and
	// truncating binary data would corrupt them.
	res.Text = TruncateToolResult(security.RedactSecrets(res.Text), r.MaxResultBytes())
	return res, nil
}
