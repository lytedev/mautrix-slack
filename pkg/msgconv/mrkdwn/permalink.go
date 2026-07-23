// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package mrkdwn

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Permalink is a parsed Slack link pointing at something inside a workspace,
// i.e. a channel or a specific message.
//
// See https://api.slack.com/messaging/retrieving#permalinks
type Permalink struct {
	TeamDomain string
	ChannelID  string
	// Timestamp is the Slack message timestamp (e.g. 1734024000.123456),
	// or empty if the link points at a channel rather than a message.
	Timestamp string
	// ThreadTimestamp is the thread root of the linked message, if the link had one.
	ThreadTimestamp string
}

// slackArchivesPathRegex matches the path of workspace-internal Slack links:
// /archives/<channel ID> optionally followed by /p<timestamp without dot>.
var slackArchivesPathRegex = regexp.MustCompile(`^/archives/([A-Za-z0-9]+)(?:/p([0-9]+))?/?$`)

const slackDomainSuffix = ".slack.com"

// slackTSFractionDigits is the number of digits after the dot in a Slack timestamp.
// Permalinks contain the timestamp with the dot stripped, so it has to be re-inserted.
const slackTSFractionDigits = 6

// ParsePermalink parses a Slack workspace-internal URL. It returns false for any
// URL that doesn't point at a channel or message in a Slack workspace.
func ParsePermalink(rawURL string) (*Permalink, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, false
	}
	teamDomain, isSlack := strings.CutSuffix(parsed.Hostname(), slackDomainSuffix)
	if !isSlack || teamDomain == "" || strings.Contains(teamDomain, ".") {
		return nil, false
	}
	match := slackArchivesPathRegex.FindStringSubmatch(parsed.Path)
	if match == nil {
		return nil, false
	}
	return &Permalink{
		TeamDomain:      teamDomain,
		ChannelID:       strings.ToUpper(match[1]),
		Timestamp:       parsePermalinkTimestamp(match[2]),
		ThreadTimestamp: parsed.Query().Get("thread_ts"),
	}, true
}

// parsePermalinkTimestamp turns the dotless timestamp in a permalink (1734024000123456)
// back into a normal Slack timestamp (1734024000.123456).
func parsePermalinkTimestamp(dotless string) string {
	if len(dotless) <= slackTSFractionDigits {
		return ""
	}
	splitAt := len(dotless) - slackTSFractionDigits
	return dotless[:splitAt] + "." + dotless[splitAt:]
}

// ResolvedLink is the Matrix equivalent of a workspace-internal Slack link.
type ResolvedLink struct {
	// URL is the matrix.to URL pointing at the bridged room or event.
	URL string
	// Label is a human-readable name for the target, to be used when the Slack
	// link didn't have any link text of its own.
	Label string
}

// ResolveInternalLink converts a workspace-internal Slack URL into a link to the
// corresponding bridged Matrix room or event. It returns nil if the URL isn't an
// internal Slack link, or if the target isn't bridged.
func (p *Params) ResolveInternalLink(ctx context.Context, rawURL string) *ResolvedLink {
	permalink, ok := ParsePermalink(rawURL)
	if !ok {
		return nil
	}
	if permalink.Timestamp != "" {
		if resolved := p.resolveMessageLink(ctx, permalink); resolved != nil {
			return resolved
		}
		// The specific message isn't bridged (or was bridged before the bridge
		// started); linking at least the right room is still better than a
		// Slack URL nobody on Matrix can open.
	}
	return p.resolveChannelLink(ctx, permalink)
}

// LinkTarget returns the href and link text to render for a Slack link with the
// given label (which may be empty), preferring the resolved Matrix target if the
// link pointed at something bridged.
func LinkTarget(rawURL, label string, resolved *ResolvedLink) (href, text string) {
	href, text = rawURL, label
	if resolved != nil {
		href = resolved.URL
	}
	// Slack sends bare links with the URL itself as the label; replacing the
	// href without replacing that would leave a Slack URL as the visible text.
	if text == "" || text == rawURL {
		if resolved != nil {
			text = resolved.Label
		} else {
			text = rawURL
		}
	}
	return
}

// LinkToHTML writes a Slack link as a Matrix HTML anchor.
func LinkToHTML(out io.Writer, rawURL, label string, resolved *ResolvedLink) {
	href, text := LinkTarget(rawURL, label, resolved)
	_, _ = fmt.Fprintf(out, `<a href="%s">%s</a>`, html.EscapeString(href), html.EscapeString(text))
}

func (p *Params) resolveMessageLink(ctx context.Context, permalink *Permalink) *ResolvedLink {
	if p.GetMessageInfo == nil {
		return nil
	}
	roomID, eventID := p.GetMessageInfo(ctx, permalink.ChannelID, permalink.Timestamp)
	if roomID == "" || eventID == "" {
		return nil
	}
	label := "message"
	if _, _, name := p.channelInfo(ctx, permalink.ChannelID); name != "" {
		label = fmt.Sprintf("message in %s", name)
	}
	return &ResolvedLink{
		URL:   roomID.EventURI(eventID, p.ServerName).MatrixToURL(),
		Label: label,
	}
}

func (p *Params) resolveChannelLink(ctx context.Context, permalink *Permalink) *ResolvedLink {
	mxid, alias, name := p.channelInfo(ctx, permalink.ChannelID)
	if name == "" {
		name = permalink.ChannelID
	}
	switch {
	case alias != "":
		return &ResolvedLink{URL: alias.URI().MatrixToURL(), Label: name}
	case mxid != "":
		return &ResolvedLink{URL: mxid.URI(p.ServerName).MatrixToURL(), Label: name}
	default:
		return nil
	}
}
