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

package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/slack-go/slack"
)

const (
	testTeamID          = "T0TEAM"
	engineeringTag      = "element.io.section.slack.T0TEAM.L1ENG"
	feedsTag            = "element.io.section.slack.T0TEAM.L2FEEDS"
	quietTag            = "element.io.section.slack.T0TEAM.L3QUIET"
	foreignSectionTag   = "element.io.section.6f1a1d0e-3c0a-4a53-9a43-2f7b2a1f0c11"
	otherTeamSectionTag = "element.io.section.slack.T0OTHER.L9WORK"
)

func standardSection(id, name, nextID string, channelIDs ...string) slackChannelSection {
	section := slackChannelSection{ID: id, Name: name, Type: "standard", NextChannelSection: nextID}
	section.ChannelIDsPage.ChannelIDs = channelIDs
	section.ChannelIDsPage.Count = len(channelIDs)
	return section
}

func typedSection(id, sectionType, nextID string, channelIDs ...string) slackChannelSection {
	section := standardSection(id, "", nextID, channelIDs...)
	section.Type = sectionType
	return section
}

func threeStandardSections() []slackChannelSection {
	return []slackChannelSection{
		standardSection("L1ENG", "Engineering", "L2FEEDS", "C1", "C2"),
		standardSection("L2FEEDS", "Feeds", "L3QUIET", "C3"),
		standardSection("L3QUIET", "Quiet", "", "C4"),
	}
}

func settingsFrom(t *testing.T, content string) map[string]json.RawMessage {
	t.Helper()
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(content), &settings); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return settings
}

type decodedSettings struct {
	SectionData map[string]map[string]any
	Order       []string
	Others      map[string]json.RawMessage
}

func decodeSettings(t *testing.T, settings map[string]json.RawMessage) decodedSettings {
	t.Helper()
	decoded := decodedSettings{Others: map[string]json.RawMessage{}}
	for key, raw := range settings {
		switch key {
		case elementCustomSectionDataKey:
			if err := json.Unmarshal(raw, &decoded.SectionData); err != nil {
				t.Fatalf("section data of the plan is not an object of objects: %v", err)
			}
		case elementOrderedCustomSectionsKey:
			if err := json.Unmarshal(raw, &decoded.Order); err != nil {
				t.Fatalf("section order of the plan is not a list of strings: %v", err)
			}
		default:
			decoded.Others[key] = raw
		}
	}
	return decoded
}

func mustPlan(t *testing.T, input sidebarSyncInput) sidebarSyncPlan {
	t.Helper()
	input.TeamID = testTeamID
	plan, err := planSidebarSync(input)
	if err != nil {
		t.Fatalf("planning the sidebar sync failed unexpectedly: %v", err)
	}
	return plan
}

func TestFirstSyncCreatesOneElementSectionPerStandardSlackSectionInSlackOrderBeforeChats(t *testing.T) {
	existing := settingsFrom(t, `{"language":"en","RoomList.OrderedCustomSections":["`+foreignSectionTag+`","chats"],"RoomList.CustomSectionData":{"`+foreignSectionTag+`":{"tag":"`+foreignSectionTag+`","name":"Mine"}}}`)

	plan := mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: existing})

	if plan.ElementSettingsToWrite == nil {
		t.Fatal("expected account data to be written because three Slack sections have no Element section yet, but the plan wrote nothing")
	}
	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	wantOrder := []string{foreignSectionTag, engineeringTag, feedsTag, quietTag, "chats"}
	if !slices.Equal(decoded.Order, wantOrder) {
		t.Errorf("expected the bridge's sections in Slack's order directly before chats and after the user's own section, so that they stay together; want %v, got %v", wantOrder, decoded.Order)
	}
	for tag, wantName := range map[string]string{engineeringTag: "Engineering", feedsTag: "Feeds", quietTag: "Quiet"} {
		entry := decoded.SectionData[tag]
		if entry["name"] != wantName || entry["tag"] != tag {
			t.Errorf("expected section data for %s to name it %q and repeat its own tag (Element ignores entries whose key and tag differ), got %v", tag, wantName, entry)
		}
		if _, hasSpace := entry["spaceId"]; hasSpace {
			t.Errorf("expected no spaceId on %s so that the section shows in Home, got %v", tag, entry)
		}
	}
	if decoded.SectionData[foreignSectionTag]["name"] != "Mine" {
		t.Errorf("expected the user's own section to survive unchanged, got %v", decoded.SectionData[foreignSectionTag])
	}
	if string(decoded.Others["language"]) != `"en"` {
		t.Errorf("expected unrelated Element settings to be written back unchanged, got %v", decoded.Others)
	}
}

func TestFirstSyncWithNoAccountDataYetStillCreatesTheSectionsAndTheirOrder(t *testing.T) {
	plan := mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: map[string]json.RawMessage{}})

	if plan.ElementSettingsToWrite == nil {
		t.Fatal("expected a first write when Element has stored no settings at all, but the plan wrote nothing")
	}
	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	if want := []string{engineeringTag, feedsTag, quietTag}; !slices.Equal(decoded.Order, want) {
		t.Errorf("expected the order %v (Element appends chats itself when it is missing), got %v", want, decoded.Order)
	}
}

func TestSyncingTheSameSectionsAgainWritesNothing(t *testing.T) {
	first := mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: map[string]json.RawMessage{}})

	second := mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: first.ElementSettingsToWrite})

	if second.ElementSettingsToWrite != nil {
		t.Errorf("expected no account data write when Element already matches Slack, because Element writes the whole blob and every extra write risks losing a concurrent change; got %s", second.ElementSettingsToWrite)
	}
}

func TestSyncingWithNoSlackSectionsAndNoBridgeSectionsWritesNothing(t *testing.T) {
	existing := settingsFrom(t, `{"language":"en"}`)

	plan := mustPlan(t, sidebarSyncInput{ElementSettings: existing})

	if plan.ElementSettingsToWrite != nil {
		t.Errorf("expected no write when there is nothing to add and nothing of the bridge's to remove, got %s", plan.ElementSettingsToWrite)
	}
}

func TestRenamingASlackSectionRenamesOnlyThatElementSection(t *testing.T) {
	first := mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: map[string]json.RawMessage{}})
	renamed := threeStandardSections()
	renamed[1].Name = "Firehose"

	plan := mustPlan(t, sidebarSyncInput{SlackSections: renamed, ElementSettings: first.ElementSettingsToWrite})

	if plan.ElementSettingsToWrite == nil {
		t.Fatal("expected a write for the rename, but the plan wrote nothing")
	}
	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	if decoded.SectionData[feedsTag]["name"] != "Firehose" {
		t.Errorf("expected the renamed section to be called Firehose, got %v", decoded.SectionData[feedsTag])
	}
	if decoded.SectionData[engineeringTag]["name"] != "Engineering" {
		t.Errorf("expected the other sections to keep their names, got %v", decoded.SectionData[engineeringTag])
	}
	if want := []string{engineeringTag, feedsTag, quietTag}; !slices.Equal(decoded.Order, want) {
		t.Errorf("expected a rename to leave the order alone, want %v, got %v", want, decoded.Order)
	}
}

func TestReorderingSlackSectionsReordersTheBridgeSectionsAmongTheirOwnPositions(t *testing.T) {
	existing := settingsFrom(t, `{"RoomList.OrderedCustomSections":["`+foreignSectionTag+`","`+engineeringTag+`","`+feedsTag+`","`+quietTag+`","chats"]}`)
	reordered := []slackChannelSection{
		standardSection("L3QUIET", "Quiet", "L1ENG"),
		standardSection("L1ENG", "Engineering", "L2FEEDS"),
		standardSection("L2FEEDS", "Feeds", ""),
	}
	existing = mustPlan(t, sidebarSyncInput{SlackSections: threeStandardSections(), ElementSettings: existing}).ElementSettingsToWrite

	plan := mustPlan(t, sidebarSyncInput{SlackSections: reordered, ElementSettings: existing})

	if plan.ElementSettingsToWrite == nil {
		t.Fatal("expected a write for the new order, but the plan wrote nothing")
	}
	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	want := []string{foreignSectionTag, quietTag, engineeringTag, feedsTag, "chats"}
	if !slices.Equal(decoded.Order, want) {
		t.Errorf("expected Slack's new order Quiet, Engineering, Feeds, with the user's section untouched before it; want %v, got %v", want, decoded.Order)
	}
}

func TestDeletingASlackSectionRemovesOnlyItsElementSectionAndKeepsEveryForeignOne(t *testing.T) {
	existing := settingsFrom(t, `{
		"RoomList.CustomSectionData":{
			"`+foreignSectionTag+`":{"tag":"`+foreignSectionTag+`","name":"Mine","spaceId":"!space:example.org"},
			"`+otherTeamSectionTag+`":{"tag":"`+otherTeamSectionTag+`","name":"Other workspace"},
			"`+engineeringTag+`":{"tag":"`+engineeringTag+`","name":"Engineering"},
			"`+feedsTag+`":{"tag":"`+feedsTag+`","name":"Feeds"}},
		"RoomList.OrderedCustomSections":["`+foreignSectionTag+`","`+engineeringTag+`","`+otherTeamSectionTag+`","`+feedsTag+`","chats"]}`)
	remaining := []slackChannelSection{
		standardSection("L1ENG", "Engineering", "", "C1"),
	}

	plan := mustPlan(t, sidebarSyncInput{SlackSections: remaining, ElementSettings: existing})

	if plan.ElementSettingsToWrite == nil {
		t.Fatal("expected a write that removes the deleted section, but the plan wrote nothing")
	}
	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	if _, stillThere := decoded.SectionData[feedsTag]; stillThere {
		t.Errorf("expected the deleted Slack section's Element section to be gone, got %v", decoded.SectionData)
	}
	for _, mustSurvive := range []string{foreignSectionTag, otherTeamSectionTag, engineeringTag} {
		if _, kept := decoded.SectionData[mustSurvive]; !kept {
			t.Errorf("expected %s to survive, because it is either still wanted or not owned by this workspace's sync; got %v", mustSurvive, decoded.SectionData)
		}
	}
	if decoded.SectionData[foreignSectionTag]["spaceId"] != "!space:example.org" {
		t.Errorf("expected the foreign section to be written back with all its fields, got %v", decoded.SectionData[foreignSectionTag])
	}
	want := []string{foreignSectionTag, engineeringTag, otherTeamSectionTag, "chats"}
	if !slices.Equal(decoded.Order, want) {
		t.Errorf("expected only the deleted section to leave the order, want %v, got %v", want, decoded.Order)
	}
}

func TestSlackSectionsThatAreNotStandardOrAreRedactedGetNoElementSection(t *testing.T) {
	redacted := standardSection("L5SECRET", "Hidden", "", "C9")
	redacted.IsRedacted = true
	sections := []slackChannelSection{
		typedSection("L6", "channels", "", "C5"),
		typedSection("L7", "direct_messages", "", "D1"),
		typedSection("L8", "slack_connect", "", "C6"),
		redacted,
		standardSection("L1ENG", "Engineering", "", "C1"),
	}

	plan := mustPlan(t, sidebarSyncInput{SlackSections: sections, ElementSettings: map[string]json.RawMessage{}})

	decoded := decodeSettings(t, plan.ElementSettingsToWrite)
	if want := []string{engineeringTag}; !slices.Equal(decoded.Order, want) {
		t.Errorf("expected only the standard, unredacted section to become an Element section, want %v, got %v", want, decoded.Order)
	}
	for _, channelID := range []string{"C5", "D1", "C6", "C9"} {
		if tag, placed := plan.SectionTagByChannelID[channelID]; placed {
			t.Errorf("expected channel %s to get no section tag so that Element's People and Chats defaults apply, got %q", channelID, tag)
		}
	}
}

func TestChannelsInTheStarsSectionGetTheFavouriteTag(t *testing.T) {
	sections := []slackChannelSection{typedSection("L0", "stars", "", "C1")}

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:       sections,
		ElementSettings:     map[string]json.RawMessage{},
		RoomTagsByChannelID: map[string][]string{"C1": {}},
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToAdd, []string{"m.favourite"}) {
		t.Errorf("expected a starred channel to gain m.favourite, got %+v", change)
	}
	if plan.ElementSettingsToWrite != nil {
		t.Errorf("expected stars to need no Element section, so no account data write; got %s", plan.ElementSettingsToWrite)
	}
}

func TestAChannelNewlyFiledInASectionGainsItsTagWhenItHasNoSectionTagYet(t *testing.T) {
	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:       threeStandardSections(),
		ElementSettings:     map[string]json.RawMessage{},
		RoomTagsByChannelID: map[string][]string{"C1": {"u.my-own-tag"}},
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToAdd, []string{engineeringTag}) || len(change.TagsToRemove) != 0 {
		t.Errorf("expected C1 to gain only the Engineering tag and keep its unrelated u.* tag, got %+v", change)
	}
}

func TestAChannelSlackMovedBetweenSectionsSwapsItsBridgeTagForTheNewOne(t *testing.T) {
	previous := map[string]string{"C1": engineeringTag, "C2": engineeringTag, "C3": feedsTag, "C4": quietTag}
	moved := []slackChannelSection{
		standardSection("L1ENG", "Engineering", "L2FEEDS", "C2"),
		standardSection("L2FEEDS", "Feeds", "L3QUIET", "C3", "C1"),
		standardSection("L3QUIET", "Quiet", "", "C4"),
	}

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:                       moved,
		ElementSettings:                     map[string]json.RawMessage{},
		RoomTagsByChannelID:                 map[string][]string{"C1": {engineeringTag}, "C2": {engineeringTag}},
		SectionTagByChannelIDAtPreviousSync: previous,
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToAdd, []string{feedsTag}) || !slices.Equal(change.TagsToRemove, []string{engineeringTag}) {
		t.Errorf("expected C1 to move from Engineering to Feeds, got %+v", change)
	}
	if unexpected, changed := plan.RoomTagChangeByChannelID["C2"]; changed {
		t.Errorf("expected C2, which Slack did not move, to be left alone, got %+v", unexpected)
	}
}

func TestAChannelSlackMovedOverridesAFavouriteTheBridgeAddedEarlierFromStars(t *testing.T) {
	previous := map[string]string{"C1": "m.favourite"}
	sections := []slackChannelSection{standardSection("L1ENG", "Engineering", "", "C1")}

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:                       sections,
		ElementSettings:                     map[string]json.RawMessage{},
		RoomTagsByChannelID:                 map[string][]string{"C1": {"m.favourite"}},
		SectionTagByChannelIDAtPreviousSync: previous,
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToRemove, []string{"m.favourite"}) || !slices.Equal(change.TagsToAdd, []string{engineeringTag}) {
		t.Errorf("expected an un-starred channel that was filed in Engineering to lose the favourite the bridge gave it and gain the Engineering tag, got %+v", change)
	}
}

func TestChannelRemovedFromAllStandardSectionsLosesItsBridgeTag(t *testing.T) {
	sections := []slackChannelSection{standardSection("L1ENG", "Engineering", "", "C2")}

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:       sections,
		ElementSettings:     map[string]json.RawMessage{},
		RoomTagsByChannelID: map[string][]string{"C1": {engineeringTag, "u.keep"}},
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToRemove, []string{engineeringTag}) || len(change.TagsToAdd) != 0 {
		t.Errorf("expected C1 to lose only its bridge tag when Slack no longer files it in a standard section, got %+v", change)
	}
}

func TestReconcileLeavesARoomTheUserMovedInElementWhereItIs(t *testing.T) {
	for name, userTags := range map[string][]string{
		"favourite":                   {"m.favourite"},
		"low priority":                {"m.lowpriority"},
		"a custom section of theirs":  {foreignSectionTag},
		"another workspace's section": {otherTeamSectionTag},
	} {
		t.Run(name, func(t *testing.T) {
			unchangedSinceLastSync := map[string]string{"C1": engineeringTag}

			plan := mustPlan(t, sidebarSyncInput{
				SlackSections:                       threeStandardSections(),
				ElementSettings:                     settingsWithTheUsersOwnSectionsDeclared(t),
				RoomTagsByChannelID:                 map[string][]string{"C1": userTags},
				SectionTagByChannelIDAtPreviousSync: unchangedSinceLastSync,
			})

			if change, changed := plan.RoomTagChangeByChannelID["C1"]; changed {
				t.Errorf("expected a room the user put in %s to stay there until Slack moves the channel again, got %+v", name, change)
			}
		})
	}
}

func settingsWithTheUsersOwnSectionsDeclared(t *testing.T) map[string]json.RawMessage {
	return settingsFrom(t, `{
		"RoomList.CustomSectionData": {
			"`+foreignSectionTag+`": {"tag": "`+foreignSectionTag+`", "name": "Mine"},
			"`+otherTeamSectionTag+`": {"tag": "`+otherTeamSectionTag+`", "name": "Other workspace"}
		},
		"RoomList.OrderedCustomSections": ["`+foreignSectionTag+`", "`+otherTeamSectionTag+`"]
	}`)
}

func TestARoomTaggedWithASectionElementNoLongerHasIsFiledLikeAnUntaggedRoom(t *testing.T) {
	sectionDeletedInElement := "element.io.section.0b8a7c1e-0000-4000-8000-deleted00000"

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:       threeStandardSections(),
		ElementSettings:     settingsWithTheUsersOwnSectionsDeclared(t),
		RoomTagsByChannelID: map[string][]string{"C1": {sectionDeletedInElement}},
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Contains(change.TagsToAdd, engineeringTag) {
		t.Errorf("expected a room whose only section tag names a section Element no longer has to be filed into its Slack section, because Element ignores such a tag and shows the room in its default section, so the user did not place it; got %+v", change)
	}
	if slices.Contains(change.TagsToRemove, sectionDeletedInElement) {
		t.Errorf("expected the bridge to leave a tag it did not create on the room, got %+v", change)
	}
}

func TestFirstSyncAfterARestartAlsoLeavesUserPlacedRoomsAlone(t *testing.T) {
	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:       threeStandardSections(),
		ElementSettings:     map[string]json.RawMessage{},
		RoomTagsByChannelID: map[string][]string{"C1": {"m.lowpriority"}},
	})

	if change, changed := plan.RoomTagChangeByChannelID["C1"]; changed {
		t.Errorf("expected a low priority room to be left alone even with no memory of the previous sync, got %+v", change)
	}
}

func TestReconcileRefilesARoomCarryingAStaleBridgeTag(t *testing.T) {
	unchangedSinceLastSync := map[string]string{"C1": engineeringTag}

	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:                       threeStandardSections(),
		ElementSettings:                     map[string]json.RawMessage{},
		RoomTagsByChannelID:                 map[string][]string{"C1": {quietTag}},
		SectionTagByChannelIDAtPreviousSync: unchangedSinceLastSync,
	})

	change := plan.RoomTagChangeByChannelID["C1"]
	if !slices.Equal(change.TagsToRemove, []string{quietTag}) || !slices.Equal(change.TagsToAdd, []string{engineeringTag}) {
		t.Errorf("expected a room still carrying a bridge tag of the wrong section to be re-filed, got %+v", change)
	}
}

func TestRoomsAlreadyInTheRightSectionProduceNoTagChanges(t *testing.T) {
	plan := mustPlan(t, sidebarSyncInput{
		SlackSections:   threeStandardSections(),
		ElementSettings: map[string]json.RawMessage{},
		RoomTagsByChannelID: map[string][]string{
			"C1": {engineeringTag}, "C3": {feedsTag}, "C9": {},
		},
		SectionTagByChannelIDAtPreviousSync: map[string]string{"C1": engineeringTag, "C3": feedsTag},
	})

	if len(plan.RoomTagChangeByChannelID) != 0 {
		t.Errorf("expected no tag changes when every room already matches Slack, got %+v", plan.RoomTagChangeByChannelID)
	}
}

func TestMissingChannelsInASectionPageAreToleratedAndOnlyTheListedChannelsAreFiled(t *testing.T) {
	section := standardSection("L1ENG", "Engineering", "", "C1", "C2")
	section.ChannelIDsPage.Count = 5

	plan := mustPlan(t, sidebarSyncInput{SlackSections: []slackChannelSection{section}, ElementSettings: map[string]json.RawMessage{}})

	if len(plan.SectionTagByChannelID) != 2 {
		t.Errorf("expected exactly the 2 listed channels of a section whose count says 5 to be filed, got %v", plan.SectionTagByChannelID)
	}
}

func TestMalformedBridgeSettingsAreRefusedInsteadOfOverwritten(t *testing.T) {
	for name, content := range map[string]string{
		"section data not an object": `{"RoomList.CustomSectionData":[1,2]}`,
		"section order not a list":   `{"RoomList.OrderedCustomSections":"chats"}`,
		"section data null":          `{"RoomList.CustomSectionData":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := planSidebarSync(sidebarSyncInput{
				TeamID:          testTeamID,
				SlackSections:   threeStandardSections(),
				ElementSettings: settingsFrom(t, content),
			})

			if err == nil {
				t.Errorf("expected an error for %s, because writing sections over data of an unknown shape could destroy the user's own sections, but planning succeeded", name)
			}
		})
	}
}

func TestSlackSectionsAreOrderedByTheirLinkedListWhateverTheResponseOrder(t *testing.T) {
	shuffled := []slackChannelSection{
		standardSection("B", "B", "C"),
		standardSection("C", "C", ""),
		standardSection("A", "A", "B"),
	}

	ordered := orderSlackSections(shuffled)

	var ids []string
	for _, section := range ordered {
		ids = append(ids, section.ID)
	}
	if want := []string{"A", "B", "C"}; !slices.Equal(ids, want) {
		t.Errorf("expected the linked list to give %v, got %v", want, ids)
	}
}

func TestSlackSectionsWithABrokenOrCyclicListAreStillAllKeptWithoutLooping(t *testing.T) {
	broken := []slackChannelSection{
		standardSection("A", "A", "MISSING"),
		standardSection("X", "X", "Y"),
		standardSection("Y", "Y", "X"),
	}

	ordered := orderSlackSections(broken)

	if len(ordered) != 3 {
		t.Errorf("expected all 3 sections to be kept even though the list is broken and cyclic, got %d: %+v", len(ordered), ordered)
	}
}

func TestEveryStandardSlackSectionEventTypeIsRegisteredWithSlackgoSoThatItIsDelivered(t *testing.T) {
	for _, eventType := range slackSidebarSectionEventTypes {
		if _, registered := slack.EventMapping[eventType]; !registered {
			t.Errorf("expected %s to be registered in slack.EventMapping, otherwise slackgo reports it as an unmapped error and the sync is never triggered", eventType)
		}
	}
}

func TestFetchingSectionsSendsTheTokenAsAFormFieldAndTheEscapedCookieAndParsesTheResponse(t *testing.T) {
	var gotToken, gotCookie, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		gotToken = request.PostForm.Get("token")
		cookie, _ := request.Cookie("d")
		if cookie != nil {
			gotCookie = cookie.Value
		}
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{"ok":true,"channel_sections":[{"channel_section_id":"L1","name":"Engineering","type":"standard","next_channel_section_id":null,"channel_ids_page":{"channel_ids":["C1"],"count":3}}]}`))
	}))
	defer server.Close()

	sections, err := fetchSlackChannelSections(context.Background(), server.Client(), server.URL+"/api/", "xoxc-token", "xoxd-a/b+c=")

	if err != nil {
		t.Fatalf("expected the fetch to succeed, got %v", err)
	}
	if gotPath != "/api/users.channelSections.list" || gotToken != "xoxc-token" {
		t.Errorf("expected a POST of token=xoxc-token to /api/users.channelSections.list, got path %q token %q", gotPath, gotToken)
	}
	if want := url.QueryEscape("xoxd-a/b+c="); gotCookie != want {
		t.Errorf("expected the d cookie to be query-escaped like slackgo's OptionCookie does (%q), got %q", want, gotCookie)
	}
	if len(sections) != 1 || sections[0].Name != "Engineering" || sections[0].NextChannelSection != "" || sections[0].ChannelIDsPage.Count != 3 {
		t.Errorf("expected one parsed Engineering section whose null next pointer is empty and whose count is 3, got %+v", sections)
	}
}

func TestFetchingSectionsReportsSlackErrorsInsteadOfReturningNoSections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer server.Close()

	_, err := fetchSlackChannelSections(context.Background(), server.Client(), server.URL+"/api/", "t", "c")

	if err == nil {
		t.Error("expected an ok:false response to be an error, because an empty list would make the sync delete every bridge section, but there was none")
	}
}
