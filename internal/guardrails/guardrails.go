// Package guardrails applies inbound safety checks before a request reaches a
// provider: prompt-injection detection and PII redaction. This is LLMGate's
// security layer — the differentiator. The checks here are intentionally
// simple and heuristic; treat them as a foundation to harden, not a finished
// product. See the README "Security" section for the roadmap.
package guardrails

import (
	"regexp"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// Guardrails runs the configured inbound filters.
type Guardrails struct {
	blockInjection bool
	redactPII      bool
}

// Result reports what the guardrails decided.
type Result struct {
	// Blocked is true when the request should be rejected outright.
	Blocked bool
	// Reason explains a block (safe to return to the caller).
	Reason string
	// Redactions counts PII spans replaced in the request.
	Redactions int
}

// New builds Guardrails from config.
func New(cfg config.Guardrails) *Guardrails {
	return &Guardrails{blockInjection: cfg.BlockPromptInjection, redactPII: cfg.RedactPII}
}

// injectionPatterns are coarse signals of a prompt-injection attempt. A real
// deployment should pair these with a classifier model and per-tenant policy.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all|any|the) (previous|prior|above) (instructions|prompts)`),
	regexp.MustCompile(`(?i)disregard (your|the) (system|previous) (prompt|instructions)`),
	regexp.MustCompile(`(?i)you are now (in )?(developer|dan|jailbreak) mode`),
	regexp.MustCompile(`(?i)reveal (your|the) (system|hidden) (prompt|instructions)`),
	regexp.MustCompile(`(?i)print (your|the) (system|initial) prompt`),
}

var piiPatterns = map[string]*regexp.Regexp{
	"EMAIL":     regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
	"CARD":      regexp.MustCompile(`\b(?:\d[ -]*?){13,16}\b`),
	"SSN":       regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
	"IP":        regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
	"PHONE_IND": regexp.MustCompile(`\b(?:\+91[\-\s]?)?[6-9]\d{9}\b`),
}

// Apply scans and, where configured, mutates req in place. When a request is
// blocked it returns Blocked=true and the caller should not forward it.
func (g *Guardrails) Apply(req *providers.Request) Result {
	var res Result
	for i := range req.Messages {
		content := req.Messages[i].Content

		if g.blockInjection && req.Messages[i].Role == "user" {
			if pat := matchInjection(content); pat != "" {
				res.Blocked = true
				res.Reason = "request blocked by prompt-injection guardrail"
				return res
			}
		}

		if g.redactPII {
			redacted, n := redact(content)
			if n > 0 {
				req.Messages[i].Content = redacted
				res.Redactions += n
			}
		}
	}
	return res
}

func matchInjection(s string) string {
	for _, p := range injectionPatterns {
		if p.MatchString(s) {
			return p.String()
		}
	}
	return ""
}

func redact(s string) (string, int) {
	count := 0
	for label, p := range piiPatterns {
		s = p.ReplaceAllStringFunc(s, func(string) string {
			count++
			return "[REDACTED_" + label + "]"
		})
	}
	return s, count
}
