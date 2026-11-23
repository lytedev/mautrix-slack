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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/slackid"
)

const (
	elementSettingsAccountDataType      = "im.vector.web.settings"
	elementCustomSectionDataKey         = "RoomList.CustomSectionData"
	elementOrderedCustomSectionsKey     = "RoomList.OrderedCustomSections"
	elementCustomSectionTagPrefix       = "element.io.section."
	elementChatsSectionTag              = "chats"
	elementFavouriteRoomTag             = "m.favourite"
	elementLowPriorityRoomTag           = "m.lowpriority"
	slackChannelSectionsEndpoint        = "https://slack.com/api/"
	slackChannelSectionsMethod          = "users.channelSections.list"
	slackChannelSectionTypeStandard     = "standard"
	slackChannelSectionTypeStars        = "stars"
	sidebarSyncDebounceWindow           = 2 * time.Second
	sidebarSyncPeriodicInterval         = 30 * time.Minute
	slackChannelSectionsRequestTimeout  = 30 * time.Second
	slackChannelSectionsMaximumBodySize = 8 << 20
)

// The first syncs run on a short schedule because on the very first connect
// the portals (and so the rooms to tag) are still being created in the
// background when the connection is established.
var sidebarSyncWarmUpDelays = []time.Duration{20 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Slack tells the client about changes to sidebar sections with these
// websocket events. slackgo does not know them, and EventMapping is exported,
// so they are registered here instead of forking slackgo.
var slackSidebarSectionEventTypes = []string{
	"channel_section_upserted",
	"channel_section_deleted",
	"channel_sections_channels_upserted",
	"channel_sections_channels_removed",
}

// slackSidebarSectionsChangedEvent carries no data the sync relies on: any of
// the four events just means that the sections should be fetched again.
type slackSidebarSectionsChangedEvent struct {
	Type string `json:"type"`
}

func init() {
	for _, eventType := range slackSidebarSectionEventTypes {
		slack.EventMapping[eventType] = slackSidebarSectionsChangedEvent{}
	}
}

type slackChannelSection struct {
	ID                 string `json:"channel_section_id"`
	Name               string `json:"name"`
	Type               string `json:"type"`
	IsRedacted         bool   `json:"is_redacted"`
	NextChannelSection string `json:"next_channel_section_id"`
	ChannelIDsPage     struct {
		ChannelIDs []string `json:"channel_ids"`
		Count      int      `json:"count"`
	} `json:"channel_ids_page"`
}

type slackChannelSectionsResponse struct {
	OK              bool                  `json:"ok"`
	Error           string                `json:"error"`
	ChannelSections []slackChannelSection `json:"channel_sections"`
}

// fetchSlackChannelSections calls users.channelSections.list the way the
// Slack web client does: the xoxc token as a form field and the d cookie.
// slackgo has no wrapper for it and its postMethod is unexported.
func fetchSlackChannelSections(ctx context.Context, httpClient *http.Client, endpoint, token, cookieToken string) ([]slackChannelSection, error) {
	ctx, cancel := context.WithTimeout(ctx, slackChannelSectionsRequestTimeout)
	defer cancel()
	form := url.Values{"token": {token}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+slackChannelSectionsMethod, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", slack.DefaultUserAgent)
	request.AddCookie(&http.Cookie{Name: "d", Value: url.QueryEscape(cookieToken)})
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	var parsed slackChannelSectionsResponse
	err = json.NewDecoder(&limitedReader{reader: response.Body, remaining: slackChannelSectionsMaximumBodySize}).Decode(&parsed)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("slack returned error %q", parsed.Error)
	}
	return parsed.ChannelSections, nil
}

type limitedReader struct {
	reader    interface{ Read([]byte) (int, error) }
	remaining int
}

func (limited *limitedReader) Read(buffer []byte) (int, error) {
	if limited.remaining <= 0 {
		return 0, errors.New("response body is larger than the allowed maximum")
	}
	if len(buffer) > limited.remaining {
		buffer = buffer[:limited.remaining]
	}
	count, err := limited.reader.Read(buffer)
	limited.remaining -= count
	return count, err
}

// orderSlackSections follows the next_channel_section_id linked list from the
// section that no other section points to. Sections that the list does not
// reach (broken links, cycles) keep their response order after the rest.
func orderSlackSections(sections []slackChannelSection) []slackChannelSection {
	sectionByID := make(map[string]slackChannelSection, len(sections))
	pointedTo := make(map[string]bool, len(sections))
	for _, section := range sections {
		sectionByID[section.ID] = section
		if section.NextChannelSection != "" {
			pointedTo[section.NextChannelSection] = true
		}
	}
	ordered := make([]slackChannelSection, 0, len(sections))
	visited := make(map[string]bool, len(sections))
	follow := func(headID string) {
		for currentID := headID; currentID != "" && !visited[currentID]; {
			current, exists := sectionByID[currentID]
			if !exists {
				return
			}
			visited[currentID] = true
			ordered = append(ordered, current)
			currentID = current.NextChannelSection
		}
	}
	for _, section := range sections {
		if !pointedTo[section.ID] {
			follow(section.ID)
		}
	}
	for _, section := range sections {
		follow(section.ID)
	}
	return ordered
}

type sidebarSyncInput struct {
	TeamID        string
	SlackSections []slackChannelSection
	// ElementSettings is the content of the im.vector.web.settings account
	// data event, with every top-level key kept as it was received.
	ElementSettings map[string]json.RawMessage
	// RoomTagsByChannelID has an entry for every bridged room that should be
	// considered, with the tags the user's account currently has on it.
	RoomTagsByChannelID map[string][]string
	// SectionTagByChannelIDAtPreviousSync is nil before the first sync. A
	// channel whose wanted tag differs from it was moved by Slack since then.
	SectionTagByChannelIDAtPreviousSync map[string]string
}

type roomTagChange struct {
	TagsToAdd    []string
	TagsToRemove []string
}

type sidebarSyncPlan struct {
	// ElementSettingsToWrite is nil when nothing the bridge owns changed.
	ElementSettingsToWrite   map[string]json.RawMessage
	RoomTagChangeByChannelID map[string]roomTagChange
	// SectionTagByChannelID is what to remember for the next sync.
	SectionTagByChannelID map[string]string
}

func bridgeOwnedSectionTagPrefix(teamID string) string {
	return elementCustomSectionTagPrefix + "slack." + teamID + "."
}

func isSectionRoomTag(tag string) bool {
	return tag == elementFavouriteRoomTag || tag == elementLowPriorityRoomTag || strings.HasPrefix(tag, elementCustomSectionTagPrefix)
}

type wantedCustomSection struct {
	Tag  string
	Name string
}

func planSidebarSync(input sidebarSyncInput) (sidebarSyncPlan, error) {
	ownedPrefix := bridgeOwnedSectionTagPrefix(input.TeamID)
	var wantedSections []wantedCustomSection
	wantedTagByChannelID := make(map[string]string)
	for _, section := range orderSlackSections(input.SlackSections) {
		var tag string
		switch section.Type {
		case slackChannelSectionTypeStandard:
			if section.IsRedacted || strings.TrimSpace(section.Name) == "" || section.ID == "" {
				continue
			}
			tag = ownedPrefix + section.ID
			wantedSections = append(wantedSections, wantedCustomSection{Tag: tag, Name: section.Name})
		case slackChannelSectionTypeStars:
			tag = elementFavouriteRoomTag
		default:
			continue
		}
		for _, channelID := range section.ChannelIDsPage.ChannelIDs {
			if _, alreadyPlaced := wantedTagByChannelID[channelID]; !alreadyPlaced {
				wantedTagByChannelID[channelID] = tag
			}
		}
	}

	plan := sidebarSyncPlan{
		RoomTagChangeByChannelID: make(map[string]roomTagChange),
		SectionTagByChannelID:    wantedTagByChannelID,
	}
	updatedSettings, err := planElementSettings(input.ElementSettings, ownedPrefix, wantedSections)
	if err != nil {
		return sidebarSyncPlan{}, err
	}
	plan.ElementSettingsToWrite = updatedSettings

	for channelID, currentTags := range input.RoomTagsByChannelID {
		change := planRoomTags(
			currentTags,
			ownedPrefix,
			wantedTagByChannelID[channelID],
			input.SectionTagByChannelIDAtPreviousSync,
			channelID,
		)
		if len(change.TagsToAdd) > 0 || len(change.TagsToRemove) > 0 {
			plan.RoomTagChangeByChannelID[channelID] = change
		}
	}
	return plan, nil
}

func planRoomTags(currentTags []string, ownedPrefix, wantedTag string, previousWantedTags map[string]string, channelID string) roomTagChange {
	var change roomTagChange
	hasTag := func(wanted string) bool { return slices.Contains(currentTags, wanted) }
	slackMovedTheChannel := false
	previousWantedTag := ""
	if previousWantedTags != nil {
		previousWantedTag = previousWantedTags[channelID]
		slackMovedTheChannel = previousWantedTag != wantedTag
	}
	ownedTags := make([]string, 0, 1)
	hasAnySectionTag := false
	for _, tag := range currentTags {
		if strings.HasPrefix(tag, ownedPrefix) {
			ownedTags = append(ownedTags, tag)
		}
		if isSectionRoomTag(tag) {
			hasAnySectionTag = true
		}
	}

	mayRefile := slackMovedTheChannel || !hasAnySectionTag || len(ownedTags) > 0
	if !mayRefile {
		return change
	}
	for _, ownedTag := range ownedTags {
		if ownedTag != wantedTag {
			change.TagsToRemove = append(change.TagsToRemove, ownedTag)
		}
	}
	// The favourite tag is shared with the user, so the bridge only takes it
	// away again when it is the one that put it there.
	if slackMovedTheChannel && previousWantedTag == elementFavouriteRoomTag && wantedTag != elementFavouriteRoomTag && hasTag(elementFavouriteRoomTag) {
		change.TagsToRemove = append(change.TagsToRemove, elementFavouriteRoomTag)
	}
	if wantedTag != "" && !hasTag(wantedTag) {
		change.TagsToAdd = append(change.TagsToAdd, wantedTag)
	}
	return change
}

func planElementSettings(existing map[string]json.RawMessage, ownedPrefix string, wantedSections []wantedCustomSection) (map[string]json.RawMessage, error) {
	existingSectionData := map[string]json.RawMessage{}
	if raw, present := existing[elementCustomSectionDataKey]; present {
		err := json.Unmarshal(raw, &existingSectionData)
		if err != nil || existingSectionData == nil {
			return nil, fmt.Errorf("%s is not an object, refusing to overwrite it: %v", elementCustomSectionDataKey, err)
		}
	}
	var existingOrder []string
	if raw, present := existing[elementOrderedCustomSectionsKey]; present {
		err := json.Unmarshal(raw, &existingOrder)
		if err != nil {
			return nil, fmt.Errorf("%s is not a list of strings, refusing to overwrite it: %w", elementOrderedCustomSectionsKey, err)
		}
	}

	wantedTags := make([]string, len(wantedSections))
	wantedTagSet := make(map[string]bool, len(wantedSections))
	for index, section := range wantedSections {
		wantedTags[index] = section.Tag
		wantedTagSet[section.Tag] = true
	}
	isOwnedAndNotWanted := func(tag string) bool {
		return strings.HasPrefix(tag, ownedPrefix) && !wantedTagSet[tag]
	}

	sectionDataChanged := false
	newSectionData := make(map[string]json.RawMessage, len(existingSectionData)+len(wantedSections))
	for tag, raw := range existingSectionData {
		if isOwnedAndNotWanted(tag) {
			sectionDataChanged = true
			continue
		}
		newSectionData[tag] = raw
	}
	for _, section := range wantedSections {
		wantedEntry := map[string]any{"tag": section.Tag, "name": section.Name}
		var currentEntry map[string]any
		if raw, present := existingSectionData[section.Tag]; present {
			_ = json.Unmarshal(raw, &currentEntry)
		}
		if !reflect.DeepEqual(currentEntry, wantedEntry) {
			sectionDataChanged = true
			encoded, err := json.Marshal(wantedEntry)
			if err != nil {
				return nil, fmt.Errorf("failed to encode section %s: %w", section.Tag, err)
			}
			newSectionData[section.Tag] = encoded
		}
	}

	orderWithoutDeletedSections := make([]string, 0, len(existingOrder))
	for _, tag := range existingOrder {
		if !isOwnedAndNotWanted(tag) {
			orderWithoutDeletedSections = append(orderWithoutDeletedSections, tag)
		}
	}
	ownedTagsInCurrentOrder := make([]string, 0, len(wantedTags))
	for _, tag := range orderWithoutDeletedSections {
		if wantedTagSet[tag] {
			ownedTagsInCurrentOrder = append(ownedTagsInCurrentOrder, tag)
		}
	}
	newOrder := orderWithoutDeletedSections
	if !slices.Equal(ownedTagsInCurrentOrder, wantedTags) {
		withoutOwnedSections := make([]string, 0, len(orderWithoutDeletedSections))
		for _, tag := range orderWithoutDeletedSections {
			if !wantedTagSet[tag] {
				withoutOwnedSections = append(withoutOwnedSections, tag)
			}
		}
		insertAt := slices.Index(withoutOwnedSections, elementChatsSectionTag)
		if insertAt < 0 {
			insertAt = len(withoutOwnedSections)
		}
		newOrder = slices.Concat(withoutOwnedSections[:insertAt], wantedTags, withoutOwnedSections[insertAt:])
	}
	orderChanged := !slices.Equal(newOrder, existingOrder)

	if !sectionDataChanged && !orderChanged {
		return nil, nil
	}
	updated := make(map[string]json.RawMessage, len(existing)+2)
	for key, raw := range existing {
		updated[key] = raw
	}
	encodedSectionData, err := json.Marshal(newSectionData)
	if err != nil {
		return nil, fmt.Errorf("failed to encode section data: %w", err)
	}
	encodedOrder, err := json.Marshal(newOrder)
	if err != nil {
		return nil, fmt.Errorf("failed to encode section order: %w", err)
	}
	updated[elementCustomSectionDataKey] = encodedSectionData
	updated[elementOrderedCustomSectionsKey] = encodedOrder
	return updated, nil
}

// sidebarMatrixClient is the part of the user's own Matrix client that the
// sidebar sync uses, so that tests can stand in for it.
type sidebarMatrixClient interface {
	GetAccountData(ctx context.Context, name string, output any) error
	SetAccountData(ctx context.Context, name string, data any) error
	GetTags(ctx context.Context, roomID id.RoomID) (event.TagEventContent, error)
	AddTagWithCustomData(ctx context.Context, roomID id.RoomID, tag event.RoomTag, data any) error
	RemoveTag(ctx context.Context, roomID id.RoomID, tag event.RoomTag) error
}

// requestSidebarSectionSync asks the sync loop for a sync soon. Many requests
// in a short time (Slack sends several events for one drag in the sidebar)
// become one sync.
func (s *SlackClient) requestSidebarSectionSync() {
	select {
	case s.sidebarSyncRequests <- struct{}{}:
	default:
	}
}

func (s *SlackClient) runSidebarSectionSyncLoop(ctx context.Context) {
	if !s.Main.Config.SyncSidebarSections || !s.IsRealUser {
		return
	}
	ctx = s.UserLogin.Log.With().Str("component", "sidebar section sync").Logger().WithContext(ctx)
	warmUpStep := 0
	nextPeriodicSync := time.NewTimer(sidebarSyncWarmUpDelays[warmUpStep])
	defer nextPeriodicSync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.sidebarSyncRequests:
			debounce := time.NewTimer(sidebarSyncDebounceWindow)
		absorbFurtherRequests:
			for {
				select {
				case <-s.sidebarSyncRequests:
				case <-debounce.C:
					break absorbFurtherRequests
				case <-ctx.Done():
					debounce.Stop()
					return
				}
			}
			s.syncSidebarSectionsAndLogFailure(ctx)
		case <-nextPeriodicSync.C:
			s.syncSidebarSectionsAndLogFailure(ctx)
			warmUpStep++
			if warmUpStep < len(sidebarSyncWarmUpDelays) {
				nextPeriodicSync.Reset(sidebarSyncWarmUpDelays[warmUpStep])
			} else {
				nextPeriodicSync.Reset(sidebarSyncPeriodicInterval)
			}
		}
	}
}

func (s *SlackClient) syncSidebarSectionsAndLogFailure(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error().Any("panic", recovered).Msg("Panic while syncing sidebar sections")
		}
	}()
	err := s.syncSidebarSections(ctx)
	if err != nil && ctx.Err() == nil {
		log.Err(err).Msg("Failed to sync sidebar sections")
	}
}

func (s *SlackClient) syncSidebarSections(ctx context.Context) error {
	log := zerolog.Ctx(ctx)
	client := s.Client
	if client == nil {
		return nil
	}
	meta := s.UserLogin.Metadata.(*slackid.UserLoginMetadata)
	if meta.CookieToken == "" || meta.Token == "" {
		log.Debug().Msg("Not syncing sidebar sections without a token and cookie")
		return nil
	}
	doublePuppet := s.UserLogin.User.DoublePuppet(ctx)
	if doublePuppet == nil {
		log.Warn().Msg("Not syncing sidebar sections without double puppeting, although sync_sidebar_sections is on")
		return nil
	}
	appserviceIntent, isAppserviceIntent := doublePuppet.(*matrix.ASIntent)
	if !isAppserviceIntent {
		log.Warn().Type("intent_type", doublePuppet).Msg("Not syncing sidebar sections although sync_sidebar_sections is on: double puppet is not an appservice intent")
		return nil
	}

	sections, err := fetchSlackChannelSections(ctx, &http.Client{}, slackChannelSectionsEndpoint, meta.Token, meta.CookieToken)
	if err != nil {
		return fmt.Errorf("failed to fetch channel sections: %w", err)
	}
	for _, section := range sections {
		if section.ChannelIDsPage.Count != len(section.ChannelIDsPage.ChannelIDs) {
			log.Debug().
				Str("section_id", section.ID).
				Str("section_type", section.Type).
				Int("count", section.ChannelIDsPage.Count).
				Int("listed_channel_ids", len(section.ChannelIDsPage.ChannelIDs)).
				Msg("Slack listed fewer channels in a section than its count")
		}
	}

	s.Main.sidebarSyncLock.Lock()
	defer s.Main.sidebarSyncLock.Unlock()
	return s.applySidebarSections(ctx, appserviceIntent.Matrix, sections)
}

func (s *SlackClient) applySidebarSections(ctx context.Context, matrixClient sidebarMatrixClient, sections []slackChannelSection) error {
	log := zerolog.Ctx(ctx)
	s.sidebarSyncStateLock.Lock()
	previousSectionTags := s.sidebarSectionTagByChannelID
	s.sidebarSyncStateLock.Unlock()

	roomIDByChannelID, err := s.roomsToConsiderForSidebarSync(ctx, sections, previousSectionTags)
	if err != nil {
		return err
	}
	roomTagsByChannelID := make(map[string][]string, len(roomIDByChannelID))
	for channelID, roomID := range roomIDByChannelID {
		tagContent, err := matrixClient.GetTags(ctx, roomID)
		if err != nil && !errors.Is(err, mautrix.MNotFound) {
			return fmt.Errorf("failed to get tags of %s: %w", roomID, err)
		}
		tagNames := make([]string, 0, len(tagContent.Tags))
		for tag := range tagContent.Tags {
			tagNames = append(tagNames, string(tag))
		}
		roomTagsByChannelID[channelID] = tagNames
	}

	// Read last: Element writes the whole event, and there is no
	// compare-and-swap, so the window between this read and the write below
	// is kept as short as it can be.
	currentSettings := map[string]json.RawMessage{}
	err = matrixClient.GetAccountData(ctx, elementSettingsAccountDataType, &currentSettings)
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		return fmt.Errorf("failed to read %s: %w", elementSettingsAccountDataType, err)
	}
	plan, err := planSidebarSync(sidebarSyncInput{
		TeamID:                              s.TeamID,
		SlackSections:                       sections,
		ElementSettings:                     currentSettings,
		RoomTagsByChannelID:                 roomTagsByChannelID,
		SectionTagByChannelIDAtPreviousSync: previousSectionTags,
	})
	if err != nil {
		return err
	}

	everyWriteSucceeded := true
	if plan.ElementSettingsToWrite != nil {
		err = matrixClient.SetAccountData(ctx, elementSettingsAccountDataType, plan.ElementSettingsToWrite)
		if err != nil {
			return fmt.Errorf("failed to write %s: %w", elementSettingsAccountDataType, err)
		}
		log.Info().Msg("Updated the Slack sections in Element's room list settings")
	}
	for channelID, change := range plan.RoomTagChangeByChannelID {
		roomID := roomIDByChannelID[channelID]
		for _, tag := range change.TagsToRemove {
			err = matrixClient.RemoveTag(ctx, roomID, event.RoomTag(tag))
			if err != nil {
				everyWriteSucceeded = false
				log.Err(err).Stringer("room_id", roomID).Str("tag", tag).Msg("Failed to remove room tag")
			}
		}
		for _, tag := range change.TagsToAdd {
			err = matrixClient.AddTagWithCustomData(ctx, roomID, event.RoomTag(tag), map[string]any{})
			if err != nil {
				everyWriteSucceeded = false
				log.Err(err).Stringer("room_id", roomID).Str("tag", tag).Msg("Failed to add room tag")
			}
		}
		log.Debug().
			Str("channel_id", channelID).
			Stringer("room_id", roomID).
			Strs("tags_added", change.TagsToAdd).
			Strs("tags_removed", change.TagsToRemove).
			Msg("Changed sidebar section tags of room")
	}
	if len(plan.RoomTagChangeByChannelID) > 0 {
		log.Info().
			Int("rooms_retagged", len(plan.RoomTagChangeByChannelID)).
			Int("slack_sections", len(sections)).
			Bool("every_write_succeeded", everyWriteSucceeded).
			Msg("Synced Slack sidebar sections into room tags")
	}
	if everyWriteSucceeded {
		s.sidebarSyncStateLock.Lock()
		s.sidebarSectionTagByChannelID = plan.SectionTagByChannelID
		s.sidebarSyncStateLock.Unlock()
	}
	return nil
}

// roomsToConsiderForSidebarSync lists the rooms whose tags could need a
// change: those of channels in a section now or at the previous sync. The
// first sync after a start looks at every portal of the login, because tags
// the bridge set in an earlier run may be on rooms that left their section.
func (s *SlackClient) roomsToConsiderForSidebarSync(ctx context.Context, sections []slackChannelSection, previousSectionTags map[string]string) (map[string]id.RoomID, error) {
	candidateChannelIDs := make(map[string]bool)
	for _, section := range sections {
		for _, channelID := range section.ChannelIDsPage.ChannelIDs {
			candidateChannelIDs[channelID] = true
		}
	}
	for channelID := range previousSectionTags {
		candidateChannelIDs[channelID] = true
	}
	if previousSectionTags == nil {
		userPortals, err := s.UserLogin.Bridge.DB.UserPortal.GetAllForLogin(ctx, s.UserLogin.UserLogin)
		if err != nil {
			return nil, fmt.Errorf("failed to list portals of the login: %w", err)
		}
		for _, userPortal := range userPortals {
			teamID, channelID := slackid.ParsePortalID(userPortal.Portal.ID)
			if teamID == s.TeamID && channelID != "" {
				candidateChannelIDs[channelID] = true
			}
		}
	}
	roomIDByChannelID := make(map[string]id.RoomID, len(candidateChannelIDs))
	for channelID := range candidateChannelIDs {
		// Whether the portal is split per login depends on the channel type
		// and the bridge config, so both keys are tried.
		for _, wantReceiver := range []bool{false, true} {
			portalKey := slackid.MakePortalKey(s.TeamID, channelID, s.UserLogin.ID, wantReceiver)
			portal, err := s.Main.br.GetExistingPortalByKey(ctx, portalKey)
			if err != nil {
				return nil, fmt.Errorf("failed to look up portal of %s: %w", channelID, err)
			}
			if portal != nil && portal.MXID != "" {
				roomIDByChannelID[channelID] = portal.MXID
				break
			}
		}
	}
	return roomIDByChannelID, nil
}
