package muster

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// What muster's sign-in challenge says in its text when its structured
// content is not there to read: the server on its "Server:" line or in the
// first sentence, and the sign-in link.
var (
	challengeServerRe = regexp.MustCompile(`(?m)^\s*Server:\s*(\S+)|server '([^']+)'`)
	challengeURLRe    = regexp.MustCompile(`https?://\S+`)
)

// jsonEscapedAmp is how Go's HTML-safe JSON encoding spells "&" inside a
// string value.
const jsonEscapedAmp = `\u0026`

// challengeURLTerminators are the JSON string escapes that end a link embedded
// in an undecoded JSON document: the escape's backslash and letter are not
// whitespace, so the URL pattern swallows them and the prose that follows.
var challengeURLTerminators = []string{`\n`, `\r`, `\t`, `\f`, `\"`}

// SignInChallenge reads muster's sign-in challenge out of a tool's answer:
// the server the person has to connect and the link where they do. It is an
// error result whose structured content says auth_required and names the
// server and the authUrl, the text saying the same (and alone, when the answer
// is call_tool refusing the call before any envelope). A challenge that names
// no server is for defaultServer. A challenge without an https link is not one
// the person can act on: ok is false, as for agent-manager's own
// "auth_required: ..." refusal.
func (r Result) SignInChallenge(defaultServer string) (server, loginURL string, ok bool) {
	if !r.IsError {
		return "", "", false
	}
	var challenge struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		AuthURL string `json:"authUrl"`
	}
	structured := len(r.Structured) > 0 && json.Unmarshal(r.Structured, &challenge) == nil && challenge.Status == "auth_required"
	if !structured {
		challenge.Server, challenge.AuthURL = "", ""
	}
	if challenge.Server == "" {
		if m := challengeServerRe.FindStringSubmatch(r.Text); m != nil {
			challenge.Server = m[1] + m[2]
		}
	}
	if !structured && challenge.Server == "" && !strings.Contains(r.Text, "auth_required") && !strings.Contains(r.Text, "Authentication Required") {
		return "", "", false
	}
	if challenge.AuthURL == "" {
		challenge.AuthURL = linkIn(r.Text)
	}
	if u, err := url.Parse(challenge.AuthURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", false
	}
	if challenge.Server == "" {
		challenge.Server = defaultServer
	}
	return challenge.Server, challenge.AuthURL, true
}

// linkIn is the first link in text, cut where it ends: at an escape of an
// undecoded JSON document, and before the prose's punctuation after it.
func linkIn(text string) string {
	link := challengeURLRe.FindString(text)
	for _, esc := range challengeURLTerminators {
		if i := strings.Index(link, esc); i >= 0 {
			link = link[:i]
		}
	}
	link = strings.ReplaceAll(link, jsonEscapedAmp, "&")
	return strings.TrimRight(link, ".,;:)]}>\"'")
}
