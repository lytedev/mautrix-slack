// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
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

package slackid

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseSlackTimestamp(t *testing.T) {
	type testCase struct {
		name     string
		input    string
		expected time.Time
	}
	testCases := []testCase{
		{"Normal", "1234567890.123456", time.Unix(1234567890, 123456000)},
		{"OffBy1-", "1234567890.12345", time.Unix(1234567890, 123450000)},
		{"OffBy1+", "1234567890.1234567", time.Unix(1234567890, 123456700)},
		{"SecondsOnly", "1234567890", time.Unix(1234567890, 0)},
		{"Millis", "1234567890.123", time.UnixMilli(1234567890123)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ParseSlackTimestamp(tc.input))
		})
	}
}

func TestSubSpacePortalIDRoundTripsForEverySubSpaceType(t *testing.T) {
	allSubSpaceTypes := []SubSpaceType{SubSpaceChannels, SubSpaceDMs, SubSpaceGroupDMs, SubSpaceHuddles}
	for _, subSpaceType := range allSubSpaceTypes {
		t.Run(string(subSpaceType), func(t *testing.T) {
			portalID := MakeSubSpacePortalID("T123", subSpaceType)

			parsedTeamID, parsedSubSpaceType, parsedOK := ParseSubSpacePortalID(portalID)

			assert.True(t, parsedOK, "a portal ID made for sub-space type %q must be recognised as a sub-space portal ID, but %q was rejected", subSpaceType, portalID)
			assert.Equal(t, "T123", parsedTeamID, "parsing %q must return the team ID it was made with", portalID)
			assert.Equal(t, subSpaceType, parsedSubSpaceType, "parsing %q must return the sub-space type it was made with", portalID)
		})
	}
}

func TestParseSubSpacePortalIDRejectsAnUnknownSubSpaceSuffix(t *testing.T) {
	portalIDWithUnknownSuffix := MakeSubSpacePortalID("T123", SubSpaceType("not-a-real-sub-space"))

	_, _, parsedOK := ParseSubSpacePortalID(portalIDWithUnknownSuffix)

	assert.False(t, parsedOK, "a portal ID with an unknown sub-space suffix must be rejected so it is treated as a normal portal, but %q was accepted", portalIDWithUnknownSuffix)
}
