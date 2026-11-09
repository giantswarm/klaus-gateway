package slack

import (
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// signInLinkText shows a link the person is asked to open before they agree
// to: the domain it opens in bold, then the whole URL as written, in code so
// Slack neither shortens nor links it. An internationalized domain is shown
// in its punycode form, with a warning that it may imitate another one. The
// link must have passed muster.ValidateSignInURL.
func signInLinkText(link string) string {
	host := ""
	if u, err := url.Parse(link); err == nil {
		host = u.Hostname()
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = host
	}
	var b strings.Builder
	b.WriteString(signInLinkOpensLead)
	b.WriteString("*" + escapeMrkdwn(ascii) + "*")
	b.WriteString("\n`" + escapeMrkdwn(link) + "`")
	if isPunycode(ascii) || ascii != host {
		b.WriteString("\n")
		b.WriteString(signInLinkPunycodeWarning)
	}
	return b.String()
}

// isPunycode reports whether a host has a label in punycode, the ASCII form
// of an internationalized domain name.
func isPunycode(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(strings.ToLower(label), "xn--") {
			return true
		}
	}
	return false
}

// signInPromptText is a sign-in prompt's text: lead (mrkdwn, escaped by the
// caller), the server's message quoted, the link as signInLinkText shows it,
// and tail when there is one.
func signInPromptText(lead, message, link, tail string) string {
	var b strings.Builder
	b.WriteString(lead)
	if message = strings.TrimSpace(message); message != "" {
		b.WriteString("\n>")
		b.WriteString(strings.ReplaceAll(escapeMrkdwn(truncateRunes(message, signInMessageMaxRunes)), "\n", "\n>"))
	}
	b.WriteString("\n")
	b.WriteString(signInLinkText(link))
	if tail != "" {
		b.WriteString("\n")
		b.WriteString(tail)
	}
	return b.String()
}
