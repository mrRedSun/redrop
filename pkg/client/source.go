package client

import "fmt"

// RenderMode selects a local publishing conversion. The zero value means auto.
type RenderMode string

const (
	RenderAuto     RenderMode = "auto"
	RenderMarkdown RenderMode = "markdown"
	RenderNone     RenderMode = "none"
)

// UsageError identifies invalid publishing options rather than runtime failures.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }

func usageErrorf(format string, args ...any) error {
	return &UsageError{Message: fmt.Sprintf(format, args...)}
}

// ParseRenderMode validates a mode and normalizes the empty default to auto.
func ParseRenderMode(value string) (RenderMode, error) {
	switch mode := RenderMode(value); mode {
	case "", RenderAuto:
		return RenderAuto, nil
	case RenderMarkdown, RenderNone:
		return mode, nil
	default:
		return "", usageErrorf("unknown render mode %q (use auto, markdown, or none)", value)
	}
}
