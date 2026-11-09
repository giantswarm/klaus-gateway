package slack

import (
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// signInLinkText shows a link the person is asked to open before they agree
// to: the domain it opens in bold, then the whole URL as written, in code so
// Slack neither shortens nor links it. An internationalized domain is shown
// in its punycode form, with a warning that it may imitate another one; one
// that has no punycode form is shown as written, with the same warning. The
// link must have passed muster.ValidateSignInURL.
func signInLinkText(link string) string {
	host := ""
	if u, err := url.Parse(link); err == nil {
		host = u.Hostname()
	}
	shown, err := idna.Lookup.ToASCII(host)
	if err != nil {
		shown = host
	}
	var b strings.Builder
	b.WriteString(signInLinkOpensLead)
	b.WriteString("*" + escapeMrkdwn(shown) + "*")
	b.WriteString("\n`" + escapeMrkdwn(link) + "`")
	if isPunycode(shown) || !isASCII(host) {
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

func isASCII(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r >= utf8.RuneSelf })
}

// signInPromptText is a sign-in prompt's text: lead (mrkdwn, escaped by the
// caller), the server's message quoted, the link as signInLinkText shows it,
// and tail when there is one. The message gets what room the section has
// left, so the link is never cut.
func signInPromptText(lead, message, link, tail string) string {
	linkText := "\n" + signInLinkText(link)
	if tail != "" {
		tail = "\n" + tail
	}
	var b strings.Builder
	b.WriteString(lead)
	if message = strings.TrimSpace(message); message != "" {
		// Quoting adds "\n>" before the message and a ">" after each newline.
		room := slackSectionTextMax - utf8.RuneCountInString(lead+linkText+tail) - 2 - strings.Count(message, "\n")
		if room = min(room, signInMessageMaxRunes); room > 1 {
			b.WriteString("\n>")
			b.WriteString(strings.ReplaceAll(escapeMrkdwnCapped(message, room), "\n", "\n>"))
		}
	}
	b.WriteString(linkText)
	b.WriteString(tail)
	return b.String()
}
