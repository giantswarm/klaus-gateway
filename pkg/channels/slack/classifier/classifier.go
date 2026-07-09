// Package classifier provides rule-based risk classification of A2A
// input-required prompts so safe tool calls can be auto-approved without
// human interaction.
package classifier

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
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

// Risk-level names, used both for String() and for parsing the threshold config.
const (
	riskNameGreen  = "green"
	riskNameYellow = "yellow"
	riskNameRed    = "red"
)

func (r Risk) String() string {
	switch r {
	case RiskGreen:
		return riskNameGreen
	case RiskYellow:
		return riskNameYellow
	default:
		return riskNameRed
	}
}

// ParseThreshold parses an auto-approve threshold string into the highest
// auto-approvable Risk. enabled is false for the disabling values
// ("off"/"none"/"disabled"); ok is false for any unrecognised value, so callers
// can reject a typo instead of silently defaulting to a permissive setting.
// "" and "green" map to read-only auto-approval.
func ParseThreshold(s string) (threshold Risk, enabled, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "disabled":
		return 0, false, true
	case "", riskNameGreen:
		return RiskGreen, true, true
	case riskNameYellow:
		return RiskYellow, true, true
	case riskNameRed:
		return RiskRed, true, true
	default:
		return 0, false, false
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

// Classifier applies risk rules to a structured tool call.
// A zero-value Classifier is valid and uses default rules with a RiskGreen threshold.
type Classifier struct {
	Config Config
}

// ClassifyTool classifies a pending tool call. RiskGreen comes from the tool
// NAME alone: every name token must be neutral, none may be a mutation verb,
// and at least one must be a known read-only verb. Argument values are
// model-controlled, so they can only escalate risk (red rules, sensitive
// paths, network hosts, mutation keywords), never reduce it; a crafted
// argument ("show", "get-token") must not steer a mutating call to green.
func (c *Classifier) ClassifyTool(name string, args map[string]any) Result {
	combined := strings.ToLower(name + " " + flattenArgs(args))

	// Red rules block auto-approval entirely, wherever they match.
	for _, rule := range redRules {
		if rule.re.MatchString(combined) {
			return Result{Risk: RiskRed, Reason: rule.reason}
		}
	}

	// Path traversal anywhere in the call.
	if strings.Contains(combined, "../") || strings.Contains(combined, "..\\") {
		return Result{Risk: RiskRed, Reason: "path traversal detected"}
	}

	// Sensitive absolute paths.
	for _, p := range sensitivePaths {
		if strings.Contains(combined, p) {
			return Result{Risk: RiskRed, Reason: "access to sensitive path: " + p}
		}
	}

	// Network operations — red only when an allowlist is configured and the host is absent.
	if reNetwork.MatchString(combined) {
		if len(c.Config.AllowedHosts) > 0 {
			host := extractHost(combined)
			if host != "" && !c.hostAllowed(host) {
				return Result{Risk: RiskRed, Reason: "network access to non-allowlisted host: " + host}
			}
		}
		return Result{Risk: RiskYellow, Reason: "network operation"}
	}

	// A mutation verb anywhere in the tool name disqualifies green, whatever
	// else the name contains ("create_or_get" is a mutation).
	tokens := nameTokens(name)
	for _, token := range tokens {
		if mutationVerbs[token] {
			return Result{Risk: RiskYellow, Reason: "mutating tool verb: " + token}
		}
	}

	// Mutation keywords in argument values escalate an otherwise-green tool
	// (e.g. a generic exec/query tool whose argument is a write statement).
	for _, rule := range yellowRules {
		if rule.re.MatchString(combined) {
			return Result{Risk: RiskYellow, Reason: rule.reason}
		}
	}

	for _, token := range tokens {
		if readOnlyVerbs[token] {
			return Result{Risk: RiskGreen, Reason: "read-only tool verb: " + token}
		}
	}

	// Unknown — escalate to be safe.
	return Result{Risk: RiskYellow, Reason: "unclassified tool"}
}

// ShouldAutoApproveTool returns true when the tool call's risk is within the
// configured threshold.
func (c *Classifier) ShouldAutoApproveTool(name string, args map[string]any) (bool, Result) {
	result := c.ClassifyTool(name, args)
	return result.Risk <= c.Config.AutoApproveThreshold, result
}

// flattenArgs joins all argument values (recursively for nested maps/lists)
// into one scan string for the escalation rules.
func flattenArgs(v any) string {
	var b strings.Builder
	appendArg(&b, v)
	return b.String()
}

func appendArg(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, inner := range t {
			appendArg(b, inner)
		}
	case []any:
		for _, inner := range t {
			appendArg(b, inner)
		}
	case nil:
	default:
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(b, "%v", t)
	}
}

// nameTokens splits a tool name into lower-cased word tokens on non-alphanumeric
// boundaries and camelCase humps, so "kubectl_get", "get-pod", "GetPod" and
// "getPodLogs" all yield a "get" token.
func nameTokens(name string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, strings.ToLower(current.String()))
			current.Reset()
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case unicode.IsUpper(r):
			if prev != 0 && !unicode.IsUpper(prev) {
				flush()
			}
			current.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return tokens
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
	{regexp.MustCompile(`\bfind\b.*\s-delete\b`), "find with -delete"},
	{regexp.MustCompile(`\bfind\b.*\s-exec(dir)?\b`), "find with -exec"},
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
	{regexp.MustCompile(`\brm\s`), "file removal"},
	{regexp.MustCompile(`>\s*/`), "shell redirect"},
}

// mutationVerbs disqualify a tool name from RiskGreen: any of these as a name
// token marks the tool side-effecting regardless of what else the name says.
var mutationVerbs = toSet(
	"create", "add", "insert", "append", "write", "put", "post", "set",
	"update", "edit", "modify", "patch", "apply", "replace", "rename", "move",
	"copy", "upload", "import", "sync", "migrate", "scale", "restart",
	"start", "stop", "pause", "resume", "run", "exec", "execute", "invoke",
	"trigger", "submit", "send", "publish", "notify", "deploy", "install",
	"uninstall", "upgrade", "rollback", "push", "commit", "merge", "rebase",
	"reset", "revert", "delete", "remove", "drop", "purge", "destroy",
	"terminate", "kill", "evict", "drain", "cordon", "uncordon", "truncate",
	"wipe", "format", "grant", "revoke", "approve", "reject", "cancel",
	"close", "open", "lock", "unlock", "enable", "disable", "assign",
	"release", "rotate", "annotate", "label", "tag", "fork", "clone",
)

// readOnlyVerbs qualify a tool name for RiskGreen when no mutation verb and no
// escalation rule fires.
var readOnlyVerbs = toSet(
	"get", "list", "read", "describe", "show", "view", "search", "find",
	"query", "lookup", "stat", "status", "check", "inspect", "explain",
	"watch", "logs", "log", "top", "diff", "count", "cat", "head", "tail",
	"ls", "grep", "history", "validate", "lint", "preview", "peek", "info",
	"summarize", "detect",
)

func toSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}
