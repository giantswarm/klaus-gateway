// Package classifier provides rule-based risk classification of A2A
// input-required prompts so safe tool calls can be auto-approved without
// human interaction.
package classifier

import (
	"fmt"
	"regexp"
	"strings"
)

// Risk indicates the danger level of a pending tool call.
type Risk int

const (
	// RiskGreen means the operation is read-only or otherwise safe to auto-approve.
	RiskGreen Risk = iota
	// RiskYellow means the operation is potentially side-effecting; human review is advised.
	RiskYellow
	// RiskRed means the operation is destructive or accesses sensitive resources;
	// auto-approval is never allowed regardless of the threshold setting.
	RiskRed
)

func (r Risk) String() string {
	switch r {
	case RiskGreen:
		return "green"
	case RiskYellow:
		return "yellow"
	default:
		return "red"
	}
}

// Result holds the classification of a single prompt.
type Result struct {
	Risk   Risk
	Reason string
}

// Config controls classifier behaviour.
type Config struct {
	// AutoApproveThreshold is the highest Risk level that may be auto-approved.
	// Defaults to RiskGreen (only explicitly safe operations).
	AutoApproveThreshold Risk
	// AllowedHosts is a list of glob patterns for network hosts the classifier
	// considers safe (contributing to RiskGreen rather than RiskYellow).
	// Patterns use simple wildcard matching: "*" matches any run of non-dot
	// characters; "**" matches everything including dots.
	AllowedHosts []string
}

// Classifier applies risk rules to a plain-text prompt.
// A zero-value Classifier is valid and uses default rules with a RiskGreen threshold.
type Classifier struct {
	Config Config
}

// Classify returns the risk level for the given prompt text.
func (c *Classifier) Classify(prompt string) Result {
	lower := strings.ToLower(prompt)

	// Red rules — checked first; any match blocks auto-approval entirely.
	for _, rule := range redRules {
		if rule.re.MatchString(lower) {
			return Result{Risk: RiskRed, Reason: rule.reason}
		}
	}

	// Path traversal anywhere in the prompt.
	if strings.Contains(prompt, "../") || strings.Contains(prompt, "..\\") {
		return Result{Risk: RiskRed, Reason: "path traversal detected"}
	}

	// Sensitive absolute paths.
	for _, p := range sensitivePaths {
		if strings.Contains(lower, p) {
			return Result{Risk: RiskRed, Reason: "access to sensitive path: " + p}
		}
	}

	// Yellow: network operations — red only when an allowlist is configured and the host is absent.
	if reNetwork.MatchString(lower) {
		if len(c.Config.AllowedHosts) > 0 {
			host := extractHost(lower)
			if host != "" && !c.hostAllowed(host) {
				return Result{Risk: RiskRed, Reason: "network access to non-allowlisted host: " + host}
			}
		}
		return Result{Risk: RiskYellow, Reason: "network operation"}
	}

	// Yellow: write / mutate operations.
	for _, rule := range yellowRules {
		if rule.re.MatchString(lower) {
			return Result{Risk: RiskYellow, Reason: rule.reason}
		}
	}

	// Bare destructive/mutating verbs that the structured yellow rules above
	// miss (they require SQL-like or tool-prefixed grammar). Without this guard
	// a prompt like "delete file X" would fall through to the broad green noun
	// rules ("file") and be auto-approved. Escalate to yellow so a human
	// reviews it -- never silently green a mutating verb.
	if reMutatingVerb.MatchString(lower) {
		return Result{Risk: RiskYellow, Reason: "mutating verb"}
	}

	// Green: read-only operations.
	for _, rule := range greenRules {
		if rule.re.MatchString(lower) {
			return Result{Risk: RiskGreen, Reason: rule.reason}
		}
	}

	// Unknown — escalate to be safe.
	return Result{Risk: RiskYellow, Reason: "unclassified operation"}
}

// ShouldAutoApprove returns true when the prompt's risk is within the configured threshold.
func (c *Classifier) ShouldAutoApprove(prompt string) (bool, Result) {
	result := c.Classify(prompt)
	return result.Risk <= c.Config.AutoApproveThreshold, result
}

// ParseRisk parses a risk-level name into a Risk. An empty string defaults to
// RiskGreen; unknown values return an error. Used to resolve the configured
// auto-approval threshold.
func ParseRisk(s string) (Risk, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "green":
		return RiskGreen, nil
	case "yellow":
		return RiskYellow, nil
	case "red":
		return RiskRed, nil
	default:
		return RiskGreen, fmt.Errorf("classifier: unknown risk level %q (want green, yellow or red)", s)
	}
}

// hostAllowed returns true when host matches one of the configured AllowedHosts globs.
func (c *Classifier) hostAllowed(host string) bool {
	for _, pattern := range c.Config.AllowedHosts {
		if matchGlob(pattern, host) {
			return true
		}
	}
	return false
}

// matchGlob implements simple wildcard matching: * = non-dot chars, ** = everything.
func matchGlob(pattern, s string) bool {
	pattern = strings.ToLower(pattern)
	s = strings.ToLower(s)
	return globMatch(pattern, s)
}

func globMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern == "**" {
		return true
	}
	if strings.HasPrefix(pattern, "**") {
		rest := pattern[2:]
		if rest == "" {
			return true
		}
		return strings.HasSuffix(s, rest)
	}
	if pattern[0] == '*' {
		// * matches any non-dot run (including empty)
		rest := pattern[1:]
		for i := 0; i <= len(s); i++ {
			if globMatch(rest, s[i:]) {
				return true
			}
			if i < len(s) && s[i] == '.' {
				break
			}
		}
		return false
	}
	if len(s) == 0 {
		return false
	}
	if pattern[0] == s[0] {
		return globMatch(pattern[1:], s[1:])
	}
	return false
}

var reNetwork = regexp.MustCompile(`(?i)(https?://|curl |wget |fetch |net\.dial|http\.get|socket\.connect)`)

// reMutatingVerb catches bare destructive/mutating verbs not covered by the
// structured yellow rules, so a destructive operation described in prose cannot
// reach the broad green noun rules.
var reMutatingVerb = regexp.MustCompile(`\b(delete|remove|destroy|drop|erase|wipe|purge|overwrite|truncate|rename|move|kill|terminate|revoke|disable|uninstall|format|reset)\b`)

var reHostInURL = regexp.MustCompile(`https?://([a-zA-Z0-9._-]+)`)

func extractHost(lower string) string {
	m := reHostInURL.FindStringSubmatch(lower)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

type ruleEntry struct {
	re     *regexp.Regexp
	reason string
}

var redRules = []ruleEntry{
	{regexp.MustCompile(`rm\s+-[a-z]*r[a-z]*\s`), "recursive delete"},
	{regexp.MustCompile(`\brm\s+--`), "rm with force flags"},
	{regexp.MustCompile(`\bmkfs\b`), "filesystem creation"},
	{regexp.MustCompile(`\bfdisk\b`), "disk partitioning"},
	{regexp.MustCompile(`\bdd\s+if=`), "raw disk write"},
	{regexp.MustCompile(`chmod\s+[0-7]*7[0-7][0-7]`), "world-writable chmod"},
	{regexp.MustCompile(`\bchown\s+root`), "chown to root"},
	{regexp.MustCompile(`\bsudo\b`), "sudo escalation"},
	{regexp.MustCompile(`\bpasswd\b`), "password modification"},
	{regexp.MustCompile(`eval\s*[(]`), "eval execution"},
	{regexp.MustCompile(`\|\s*sh\b`), "pipe to shell"},
	{regexp.MustCompile(`\|\s*bash\b`), "pipe to bash"},
	{regexp.MustCompile(`>\s*/etc/`), "write to /etc/"},
	{regexp.MustCompile(`>\s*/root/`), "write to /root/"},
	{regexp.MustCompile(`>\s*/boot/`), "write to /boot/"},
	{regexp.MustCompile(`>\s*/proc/`), "write to /proc/"},
	{regexp.MustCompile(`>\s*/sys/`), "write to /sys/"},
}

var sensitivePaths = []string{
	"/etc/passwd", "/etc/shadow", "/etc/sudoers", "/etc/ssh/",
	"/root/", "/.ssh/", "/.gnupg/", "/.aws/credentials",
	"/boot/", "/proc/", "/sys/kernel/",
}

var yellowRules = []ruleEntry{
	{regexp.MustCompile(`\b(write|create|mkdir|touch|append|truncate)\b`), "file write operation"},
	{regexp.MustCompile(`\b(exec|spawn|popen|subprocess)\b`), "process execution"},
	{regexp.MustCompile(`\b(insert|update|delete)\s+\w+\s+(into|from|set)\b`), "database mutation"},
	{regexp.MustCompile(`\b(git\s+(push|commit|rebase|reset))\b`), "git mutation"},
	{regexp.MustCompile(`\b(kubectl\s+(apply|delete|patch|create))\b`), "kubernetes mutation"},
	{regexp.MustCompile(`\bhelm\s+(install|upgrade|uninstall)\b`), "helm mutation"},
}

var greenRules = []ruleEntry{
	{regexp.MustCompile(`\b(ls|dir|list|cat|head|tail|grep|find|stat|file)\b`), "read-only file operation"},
	{regexp.MustCompile(`\b(read|open|load|parse|get|fetch|describe|show|display)\b`), "read operation"},
	{regexp.MustCompile(`\b(echo|printf|print|log)\b`), "output operation"},
	{regexp.MustCompile(`\b(git\s+(log|diff|status|show|branch|tag))\b`), "read-only git operation"},
	{regexp.MustCompile(`\b(kubectl\s+(get|describe|logs|top))\b`), "read-only kubernetes operation"},
}
