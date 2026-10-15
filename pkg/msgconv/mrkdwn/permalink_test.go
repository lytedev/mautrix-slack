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

package mrkdwn_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/msgconv/mrkdwn"
)

const (
	bridgedChannelID   = "C0123456789"
	bridgedChannelName = "#general"
	bridgedRoomID      = id.RoomID("!general:example.com")
	bridgedTimestamp   = "1734024000.123456"
	bridgedEventID     = id.EventID("$abc123")
	serverName         = "example.com"
)

func testParams() *mrkdwn.Params {
	return &mrkdwn.Params{
		ServerName: serverName,
		GetUserInfo: func(ctx context.Context, userID string) (id.UserID, string) {
			return "", ""
		},
		GetChannelInfo: func(ctx context.Context, channelID string) (id.RoomID, id.RoomAlias, string) {
			if channelID != bridgedChannelID {
				return "", "", ""
			}
			return bridgedRoomID, "", bridgedChannelName
		},
		GetMessageInfo: func(ctx context.Context, channelID, timestamp string) (id.RoomID, id.EventID) {
			if channelID != bridgedChannelID || timestamp != bridgedTimestamp {
				return "", ""
			}
			return bridgedRoomID, bridgedEventID
		},
	}
}

func TestParsePermalink(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		expected  *mrkdwn.Permalink
		expectsOK bool
	}{{
		name:      "message",
		url:       "https://example.slack.com/archives/C0123456789/p1734024000123456",
		expected:  &mrkdwn.Permalink{TeamDomain: "example", ChannelID: "C0123456789", Timestamp: "1734024000.123456"},
		expectsOK: true,
	}, {
		name: "thread reply",
		url:  "https://example.slack.com/archives/C0123456789/p1734024000123456?thread_ts=1734020000.000100&cid=C0123456789",
		expected: &mrkdwn.Permalink{
			TeamDomain: "example", ChannelID: "C0123456789",
			Timestamp: "1734024000.123456", ThreadTimestamp: "1734020000.000100",
		},
		expectsOK: true,
	}, {
		name:      "channel",
		url:       "https://example.slack.com/archives/C0123456789",
		expected:  &mrkdwn.Permalink{TeamDomain: "example", ChannelID: "C0123456789"},
		expectsOK: true,
	}, {
		name:      "channel with trailing slash",
		url:       "https://example.slack.com/archives/C0123456789/",
		expected:  &mrkdwn.Permalink{TeamDomain: "example", ChannelID: "C0123456789"},
		expectsOK: true,
	}, {
		name:      "non-archives slack link",
		url:       "https://example.slack.com/team/U0123456789",
		expectsOK: false,
	}, {
		name:      "slack.com without a workspace",
		url:       "https://slack.com/archives/C0123456789",
		expectsOK: false,
	}, {
		name:      "lookalike domain",
		url:       "https://example.slack.com.evil.example/archives/C0123456789",
		expectsOK: false,
	}, {
		name:      "unrelated link",
		url:       "https://example.com/archives/C0123456789/p1734024000123456",
		expectsOK: false,
	}, {
		name:      "not a URL",
		url:       "hello world",
		expectsOK: false,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, ok := mrkdwn.ParsePermalink(test.url)
			assert.Equal(t, test.expectsOK, ok)
			if test.expectsOK {
				assert.Equal(t, test.expected, parsed)
			}
		})
	}
}

func TestResolveInternalLink(t *testing.T) {
	params := testParams()
	ctx := context.Background()

	t.Run("bridged message", func(t *testing.T) {
		resolved := params.ResolveInternalLink(ctx, "https://example.slack.com/archives/C0123456789/p1734024000123456")
		require.NotNil(t, resolved)
		assert.Equal(t, bridgedRoomID.EventURI(bridgedEventID, serverName).MatrixToURL(), resolved.URL)
		assert.Equal(t, "message in #general", resolved.Label)
	})

	t.Run("bridged channel", func(t *testing.T) {
		resolved := params.ResolveInternalLink(ctx, "https://example.slack.com/archives/C0123456789")
		require.NotNil(t, resolved)
		assert.Equal(t, bridgedRoomID.URI(serverName).MatrixToURL(), resolved.URL)
		assert.Equal(t, bridgedChannelName, resolved.Label)
	})

	t.Run("unbridged message falls back to the room", func(t *testing.T) {
		resolved := params.ResolveInternalLink(ctx, "https://example.slack.com/archives/C0123456789/p1700000000000001")
		require.NotNil(t, resolved)
		assert.Equal(t, bridgedRoomID.URI(serverName).MatrixToURL(), resolved.URL)
	})

	t.Run("unbridged channel is left alone", func(t *testing.T) {
		assert.Nil(t, params.ResolveInternalLink(ctx, "https://example.slack.com/archives/C9999999999/p1734024000123456"))
	})

	t.Run("external link is left alone", func(t *testing.T) {
		assert.Nil(t, params.ResolveInternalLink(ctx, "https://example.com/hello"))
	})
}

func TestLinkTarget(t *testing.T) {
	const slackURL = "https://example.slack.com/archives/C0123456789"
	resolved := &mrkdwn.ResolvedLink{URL: "https://matrix.to/#/!general:example.com", Label: bridgedChannelName}

	t.Run("bare link uses the resolved label", func(t *testing.T) {
		href, text := mrkdwn.LinkTarget(slackURL, slackURL, resolved)
		assert.Equal(t, resolved.URL, href)
		assert.Equal(t, bridgedChannelName, text)
	})

	t.Run("custom label is kept", func(t *testing.T) {
		href, text := mrkdwn.LinkTarget(slackURL, "look here", resolved)
		assert.Equal(t, resolved.URL, href)
		assert.Equal(t, "look here", text)
	})

	t.Run("unresolved link is unchanged", func(t *testing.T) {
		href, text := mrkdwn.LinkTarget(slackURL, "", nil)
		assert.Equal(t, slackURL, href)
		assert.Equal(t, slackURL, text)
	})
}

func TestParseMessageLink(t *testing.T) {
	parsed, err := mrkdwn.New(testParams()).Parse(
		context.Background(),
		"see <https://example.slack.com/archives/C0123456789/p1734024000123456> and <https://example.com|elsewhere>",
		&event.Mentions{},
	)
	require.NoError(t, err)
	assert.Equal(
		t,
		`see <a href="https://matrix.to/#/%21general:example.com/$abc123?via=example.com">message in #general</a> `+
			`and <a href="https://example.com">elsewhere</a>`,
		parsed,
	)
}
