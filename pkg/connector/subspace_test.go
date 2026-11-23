package connector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/slackid"
)

func TestIsHuddleNotesConversationName(t *testing.T) {
	type nameCase struct {
		testName                string
		conversationName        string
		expectedToBeHuddleNotes bool
	}
	nameCases := []nameCase{
		{"plain_huddle_notes_name_is_huddle_notes", ":headphones: Huddle notes: 2026-11-02 in <#C0123456789>", true},
		{"ai_notes_and_transcript_name_is_huddle_notes", ":headphones: Huddle AI notes & transcript: 2026-11-02 in <#C0123456789>", true},
		{"name_with_one_leading_hash_is_huddle_notes", "#:headphones: Huddle notes: 2026-11-02 in <#C0123456789>", true},
		{"name_without_the_parent_channel_mention_is_huddle_notes", ":headphones: Huddle notes: 2026-11-02", true},
		{"ordinary_channel_is_not_huddle_notes", "general", false},
		{"channel_that_merely_mentions_huddles_is_not_huddle_notes", "huddle-planning", false},
		{"channel_named_huddle_notes_is_not_huddle_notes", "#huddle-notes", false},
		{"empty_name_is_not_huddle_notes", "", false},
		{"two_leading_hashes_is_not_huddle_notes", "##:headphones: Huddle notes: x", false},
	}
	for _, nameCase := range nameCases {
		t.Run(nameCase.testName, func(t *testing.T) {
			actual := isHuddleNotesConversationName(nameCase.conversationName)
			assert.Equal(t, nameCase.expectedToBeHuddleNotes, actual,
				"conversation name %q: expected huddle-notes classification %v because only names starting with the huddle prefix (after at most one leading #) are canvas conversations, but got %v",
				nameCase.conversationName, nameCase.expectedToBeHuddleNotes, actual)
		})
	}
}

func TestSubSpaceTypeForConversation(t *testing.T) {
	huddleNotesName := ":headphones: Huddle notes: 2026-11-02 in <#C0123456789>"
	type typeCase struct {
		testName         string
		roomType         database.RoomType
		conversationName string
		expected         slackid.SubSpaceType
	}
	typeCases := []typeCase{
		{"direct_message_goes_to_dms", database.RoomTypeDM, "", slackid.SubSpaceDMs},
		{"group_direct_message_goes_to_group_dms", database.RoomTypeGroupDM, "alice, bob", slackid.SubSpaceGroupDMs},
		{"ordinary_channel_goes_to_channels", database.RoomTypeDefault, "general", slackid.SubSpaceChannels},
		{"huddle_notes_channel_goes_to_huddles", database.RoomTypeDefault, huddleNotesName, slackid.SubSpaceHuddles},
		{"direct_message_stays_in_dms_even_with_a_huddle_notes_name", database.RoomTypeDM, huddleNotesName, slackid.SubSpaceDMs},
		{"group_direct_message_stays_in_group_dms_even_with_a_huddle_notes_name", database.RoomTypeGroupDM, huddleNotesName, slackid.SubSpaceGroupDMs},
	}
	for _, typeCase := range typeCases {
		t.Run(typeCase.testName, func(t *testing.T) {
			actual := subSpaceTypeForConversation(typeCase.roomType, typeCase.conversationName)
			assert.Equal(t, typeCase.expected, actual,
				"room type %q named %q: expected sub-space %q because DMs and group DMs are routed by type before names are considered, but got %q",
				typeCase.roomType, typeCase.conversationName, typeCase.expected, actual)
		})
	}
}

func TestHuddlesSubSpaceHasADisplayName(t *testing.T) {
	assert.Equal(t, "Huddles", subSpaceNames[slackid.SubSpaceHuddles],
		"the huddles sub-space must be created with the name Huddles, otherwise its room would have an empty name")
}

func portalForSelection(portalID string, matrixRoomID string, name string) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: networkid.PortalID(portalID)},
		MXID:      id.RoomID(matrixRoomID),
		Name:      name,
	}}
}

func TestSelectPortalsWithMatrixRoomAndHuddleNotesName(t *testing.T) {
	huddleNotesWithRoom := portalForSelection("T1-C1", "!one:example.com", ":headphones: Huddle notes: 2026-11-02 in <#C9>")
	aiNotesWithRoom := portalForSelection("T1-C2", "!two:example.com", "#:headphones: Huddle AI notes & transcript: 2026-11-02 in <#C9>")
	huddleNotesWithoutRoom := portalForSelection("T1-C3", "", ":headphones: Huddle notes: 2026-11-03 in <#C9>")
	ordinaryChannelWithRoom := portalForSelection("T1-C4", "!four:example.com", "general")

	selected := selectPortalsWithMatrixRoomAndHuddleNotesName([]*bridgev2.Portal{
		huddleNotesWithRoom, ordinaryChannelWithRoom, huddleNotesWithoutRoom, aiNotesWithRoom,
	})

	assert.Equal(t, []*bridgev2.Portal{huddleNotesWithRoom, aiNotesWithRoom}, selected,
		"only portals that have a Matrix room and a huddle-notes name must be selected for moving, in their original order; rooms that do not exist yet are created under the right parent, and ordinary channels must stay")
}

func TestSelectPortalsWithMatrixRoomAndHuddleNotesNameSelectsNothingFromNothing(t *testing.T) {
	selected := selectPortalsWithMatrixRoomAndHuddleNotesName(nil)

	assert.Empty(t, selected, "no candidate portals means nothing to move, but got %v", selected)
}
