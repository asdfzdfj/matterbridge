package bslack

import (
	"errors"
	"fmt"
	"html"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/matterbridge-org/matterbridge/bridge/helper"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ErrEventIgnored is for events that should be ignored
var ErrEventIgnored = errors.New("this event message should ignored")

func (b *Bslack) handleSlack() {
	messages := make(chan *config.Message)
	if b.GetString(incomingWebhookConfig) != "" && b.GetString(tokenConfig) == "" {
		b.Log.Debugf("Choosing webhooks based receiving")
		go b.handleMatterHook(messages)
	} else if b.GetString(appTokenConfig) != "" && b.GetString(tokenConfig) != "" {
		b.Log.Debugf("Choosing socket mode Events API based receiving")
		go b.handleSlackClientEAPI(messages)
	} else {
		b.Log.Debugf("Choosing RTM based receiving")
		go b.handleSlackClientRTM(messages)
	}
	time.Sleep(time.Second)
	b.Log.Debug("Start listening for Slack messages")
	for message := range messages {
		// don't do any action on deleted/typing messages
		if message.Event != config.EventUserTyping && message.Event != config.EventMsgDelete &&
			message.Event != config.EventFileDelete {
			b.Log.Debugf("<= Sending message from %s on %s to gateway", message.Username, b.Account)
			// cleanup the message
			message.Text = b.replaceMention(message.Text)
			message.Text = b.replaceVariable(message.Text)
			message.Text = b.replaceChannel(message.Text)
			message.Text = b.replaceURL(message.Text)
			message.Text = b.replaceb0rkedMarkDown(message.Text)
			message.Text = html.UnescapeString(message.Text)

			// Add the avatar
			message.Avatar = b.users.getAvatar(message.UserID)
		}

		b.Log.Debugf("<= Message is %#v", message)
		b.Remote <- *message
	}
}

func (b *Bslack) handleSlackClientEAPI(messages chan *config.Message) {
	b.Log.Warn("Socket mode Events API is currently WORK IN PROGRESS")

	// based on minimal socketmode example
	// see: https://github.com/slack-go/slack/blob/65e1413a1305f1fa863f662371fe6b34a12ca341/examples/socketmode/socketmode.go#L51
	for sockEvt := range b.smc.Events {
		b.Log.Debugf("== Received socket event: %#v", sockEvt)

		switch sockEvt.Type {
		case socketmode.EventTypeConnecting:
			b.Log.Info("Connecting to Slack with socket mode")
		case socketmode.EventTypeConnectionError:
			b.Log.Error("Socket mode connection failed")
		case socketmode.EventTypeConnected:
			b.Log.Info("Socket mode connected")
			si, err := b.sc.AuthTest()
			if err != nil {
				b.Log.Errorf("Slack AuthTest failed %#v", err)
				continue
			}
			b.actingUserID = si.UserID
			b.actingUserName = si.User
			b.channels.populateChannels(true)
			b.users.populateUsers(true)
		case socketmode.EventTypeHello:
			appID := sockEvt.Request.ConnectionInfo.AppID
			b.Log.Debugf("Hello received, AppID %v", appID)
		case socketmode.EventTypeInvalidAuth:
			b.Log.Fatalf("Invalid Token %#v", *sockEvt.Request)

		case socketmode.EventTypeEventsAPI:
			oevt, ok := sockEvt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				b.Log.Debugf("Ignored %#v", sockEvt)
				continue
			}
			// you MUST ack event
			b.smc.Ack(*sockEvt.Request)

			b.Log.Debugf("Received event: %v %#v", oevt.Type, oevt.Data)

			// CallbackEvent is more or less the only meaningful type of (outer) event
			if oevt.Type != slackevents.CallbackEvent {
				b.smc.Debugf("unsupported Events API event type %s received", oevt.Type)
				continue
			}

			ievt := oevt.InnerEvent
			b.Log.Debugf("inner event %#v", ievt)

			switch ev := ievt.Data.(type) {
			case *slackevents.MessageEvent:
				// for consideration: re-unmarshal incoming data to slack.MessageEvent
				// then simply feed it back into the existing legacy RTM pipeline
				// https://github.com/tippl/matterbridge/commit/42e75e2abff3627e5dd7c5de6eb42bbaa08236f8
				if b.skipMessageEventEAPI(ev) {
					b.Log.Debugf("Skipped message: %#v", ev)
					continue
				}
				rmsg, err := b.handleMessageEventEAPI(ev)
				if err != nil {
					b.Log.Errorf("%#v", err)
					continue
				}
				messages <- rmsg

			case *slackevents.FileDeletedEvent:
				rmsg, err := b.handleFileDeletedEvent(ev.FileID)
				if err != nil {
					b.Log.Infof("%#v", err)
					continue
				}
				messages <- rmsg

			case *slackevents.MemberJoinedChannelEvent:
				b.Log.Debugf("user %q joined to channel %q", ev.User, ev.Channel)
				// data in event contains only IDs
				ch, err := b.sc.GetConversationInfo(&slack.GetConversationInfoInput{
					ChannelID: ev.Channel,
				})
				if err != nil {
					b.Log.Errorf("GetConversationInfo failed %v", err)
					continue
				}
				b.channels.registerChannel(*ch)
				b.users.populateUser(ev.User)

			case *slackevents.UserChangeEvent:
				b.users.invalidateUser(ev.User.ID)
			}

		case socketmode.EventTypeSlashCommand, socketmode.EventTypeInteractive:
			// we don't really expect these events to showed up here but it's a valid one
			// so ack it properly and do nothing else just in case
			b.Log.Debugf("Skip unsupported event type %s", sockEvt.Type)
			b.smc.Ack(*sockEvt.Request)

		default:
			b.Log.Debugf("Unhandled socket event: %T", sockEvt)
		}
	}
}

func (b *Bslack) handleSlackClientRTM(messages chan *config.Message) {
	for msg := range b.rtm.IncomingEvents {
		if msg.Type != sUserTyping && msg.Type != sHello && msg.Type != sLatencyReport {
			b.Log.Debugf("== Receiving event %#v", msg.Data)
		}
		switch ev := msg.Data.(type) {
		case *slack.UserTypingEvent: // no equivalent in EAPI
			if !b.GetBool("ShowUserTyping") {
				continue
			}
			rmsg, err := b.handleTypingEvent(ev)
			if err == ErrEventIgnored {
				continue
			} else if err != nil {
				b.Log.Errorf("%#v", err)
				continue
			}
			messages <- rmsg

		case *slack.MessageEvent:
			if b.skipMessageEventRTM(ev) {
				b.Log.Debugf("Skipped message: %#v", ev)
				continue
			}
			rmsg, err := b.handleMessageEventRTM(ev)
			if err != nil {
				b.Log.Errorf("%#v", err)
				continue
			}
			messages <- rmsg
		case *slack.FileDeletedEvent:
			rmsg, err := b.handleFileDeletedEvent(ev.FileID)
			if err != nil {
				b.Log.Infof("%#v", err)
				continue
			}
			messages <- rmsg

		case *slack.ChannelJoinedEvent:
			// When we join a channel we update the full list of users as
			// well as the information for the channel that we joined as this
			// should now tell that we are a member of it.
			b.channels.registerChannel(ev.Channel)
		case *slack.MemberJoinedChannelEvent:
			b.users.populateUser(ev.User)
		case *slack.UserChangeEvent:
			b.users.invalidateUser(ev.User.ID)
		case *slack.ConnectedEvent:
			b.actingUserID = ev.Info.User.ID
			b.actingUserName = ev.Info.User.Name
			b.channels.populateChannels(true)
			b.users.populateUsers(true)

		case *slack.OutgoingErrorEvent:
			b.Log.Debugf("%#v", ev.Error())
		case *slack.InvalidAuthEvent:
			b.Log.Fatalf("Invalid Token %#v", ev)
		case *slack.ConnectionErrorEvent:
			b.Log.Errorf("Connection failed %#v %#v", ev.Error(), ev.ErrorObj)

		case *slack.HelloEvent, *slack.LatencyReport, *slack.ConnectingEvent:
			continue
		default:
			b.Log.Debugf("Unhandled incoming event: %T", ev)
		}
	}
}

func (b *Bslack) handleMatterHook(messages chan *config.Message) {
	for {
		message := b.mh.Receive()
		b.Log.Debugf("receiving from matterhook (slack) %#v", message)
		if message.UserName == "slackbot" {
			continue
		}
		messages <- &config.Message{
			Username: message.UserName,
			Text:     message.Text,
			Channel:  message.ChannelName,
		}
	}
}

func (b *Bslack) skipMessageEventRTM(ev *slack.MessageEvent) bool {
	return b.skipMessageEvent(&ev.Msg, ev.SubMessage)
}

func (b *Bslack) skipMessageEventEAPI(ev *slackevents.MessageEvent) bool {
	return b.skipMessageEvent(ev.Message, ev.PreviousMessage)
}

// skipMessageEvent skips event that need to be skipped :-)
func (b *Bslack) skipMessageEvent(msg *slack.Msg, submsg *slack.Msg) bool {
	switch msg.SubType {
	case sChannelLeave, sChannelJoin:
		return b.GetBool(noSendJoinConfig)
	case sPinnedItem, sUnpinnedItem:
		return true
	case sChannelTopic, sChannelPurpose:
		// Skip the event if our bot/user account changed the topic/purpose
		if msg.User == b.actingUserID {
			return true
		}
	}

	// Check for our callback ID
	hasOurCallbackID := false
	if len(msg.Blocks.BlockSet) == 1 {
		block, ok := msg.Blocks.BlockSet[0].(*slack.SectionBlock)
		hasOurCallbackID = ok && block.BlockID == "matterbridge_"+b.uuid
	}

	if submsg != nil {
		// It seems submsg.Edited == nil when slack unfurls.
		// Do not forward these messages. See Github issue #266.
		if submsg.ThreadTimestamp != submsg.Timestamp &&
			submsg.Edited == nil {
			return true
		}
		// see hidden subtypes at https://api.slack.com/events/message
		// these messages are sent when we add a message to a thread #709
		if msg.SubType == "message_replied" && msg.Hidden {
			return true
		}
		if len(submsg.Blocks.BlockSet) == 1 {
			block, ok := submsg.Blocks.BlockSet[0].(*slack.SectionBlock)
			hasOurCallbackID = ok && block.BlockID == "matterbridge_"+b.uuid
		}
	}

	// Skip any messages that we made ourselves or from 'slackbot' (see #527).
	if msg.Username == sSlackBotUser ||
		(b.rtm != nil && msg.Username == b.actingUserName) || hasOurCallbackID {
		return true
	}

	if len(msg.Files) > 0 {
		return b.filesCached(msg.Files)
	}
	return false
}

func (b *Bslack) filesCached(files []slack.File) bool {
	for i := range files {
		if !b.fileCached(&files[i]) {
			return false
		}
	}
	return true
}

// handleMessageEvent handles the message events. Together with any called sub-methods,
// this method implements the following event processing pipeline:
//
//  1. Check if the message should be ignored.
//     NOTE: This is not actually part of the method below but is done just before it
//     is called via the 'skipMessageEvent()' method.
//  2. Populate the Matterbridge message that will be sent to the router based on the
//     received event and logic that is common to all events that are not skipped.
//  3. Detect and handle any message that is "status" related (think join channel, etc.).
//     This might result in an early exit from the pipeline and passing of the
//     pre-populated message to the Matterbridge router.
//  4. Handle the specific case of messages that edit existing messages depending on
//     configuration.
//  5. Handle any attachments of the received event.
//  6. Check that the Matterbridge message that we end up with after at the end of the
//     pipeline is valid before sending it to the Matterbridge router.
func (b *Bslack) handleMessageEvent(msg *slack.Msg, submsg *slack.Msg) (*config.Message, error) {
	rmsg, err := b.populateReceivedMessage(msg, submsg, submsg != nil)
	if err != nil {
		return nil, err
	}

	// Handle some message types early.
	if b.handleStatusEvent(msg, submsg, rmsg) {
		return rmsg, nil
	}

	b.handleAttachments(msg, rmsg)

	// Verify that we have the right information and the message
	// is well-formed before sending it out to the router.
	if len(msg.Files) == 0 && (rmsg.Text == "" || rmsg.Username == "") {
		if msg.BotID != "" {
			// This is probably a webhook we couldn't resolve.
			return nil, fmt.Errorf("message handling resulted in an empty bot message (probably an incoming webhook we couldn't resolve): %#v", msg)
		}
		if submsg != nil {
			return nil, fmt.Errorf("message handling resulted in an empty message: %#v with submessage: %#v", msg, submsg)
		}
		return nil, fmt.Errorf("message handling resulted in an empty message: %#v", msg)
	}
	return rmsg, nil
}

func (b *Bslack) handleMessageEventRTM(ev *slack.MessageEvent) (*config.Message, error) {
	return b.handleMessageEvent(&ev.Msg, ev.SubMessage)
}

func (b *Bslack) handleMessageEventEAPI(ev *slackevents.MessageEvent) (*config.Message, error) {
	// NOTE: unsure if slackevents.MesaageEvent.PreviousMessage and slack.MessageEvent.SubMessage is equivalent here
	return b.handleMessageEvent(ev.Message, ev.PreviousMessage)
}

func (b *Bslack) handleFileDeletedEvent(fileID string) (*config.Message, error) {
	if rawChannel, ok := b.cache.Get(cfileDownloadChannel + fileID); ok {
		channel, err := b.channels.getChannelByID(rawChannel.(string))
		if err != nil {
			return nil, err
		}

		return &config.Message{
			Event:    config.EventFileDelete,
			Text:     config.EventFileDelete,
			Channel:  channel.Name,
			Account:  b.Account,
			ID:       fileID,
			Protocol: b.Protocol,
		}, nil
	}

	return nil, fmt.Errorf("channel ID for file ID %s not found", fileID)
}

func (b *Bslack) handleStatusEvent(msg *slack.Msg, submsg *slack.Msg, rmsg *config.Message) bool {
	switch msg.SubType {
	case sChannelJoined, sMemberJoined:
		// There's no further processing needed on channel events
		// so we return 'true'.
		return true
	case sChannelJoin, sChannelLeave:
		rmsg.Username = sSystemUser
		rmsg.Event = config.EventJoinLeave
	case sChannelTopic, sChannelPurpose:
		b.channels.populateChannels(false)
		rmsg.Event = config.EventTopicChange
	case sMessageChanged:
		// handle deleted thread starting messages
		if submsg.Text == "This message was deleted." {
			rmsg.Event = config.EventMsgDelete
			return true
		}
	case sMessageDeleted:
		rmsg.Text = config.EventMsgDelete
		rmsg.Event = config.EventMsgDelete
		rmsg.ID = msg.DeletedTimestamp
		// If a message is being deleted we do not need to process
		// the event any further so we return 'true'.
		return true
	case sMeMessage:
		rmsg.Event = config.EventUserAction
	}
	return false
}

func getMessageTitle(attach *slack.Attachment) string {
	if attach.TitleLink != "" {
		return fmt.Sprintf("[%s](%s)\n", attach.Title, attach.TitleLink)
	}
	return attach.Title
}

func (b *Bslack) handleAttachments(msg *slack.Msg, rmsg *config.Message) {
	// File comments are set by the system (because there is no username given).
	if msg.SubType == sFileComment {
		rmsg.Username = sSystemUser
	}

	// See if we have some text in the attachments.
	if rmsg.Text == "" {
		for i, attach := range msg.Attachments {
			if attach.Text != "" {
				if attach.Title != "" {
					rmsg.Text = getMessageTitle(&msg.Attachments[i])
				}
				rmsg.Text += attach.Text
				if attach.Footer != "" {
					rmsg.Text += "\n\n" + attach.Footer
				}
			} else {
				rmsg.Text = attach.Fallback
			}
		}
	}

	// Save the attachments, so that we can send them to other slack (compatible) bridges.
	if len(msg.Attachments) > 0 {
		rmsg.Extra[sSlackAttachment] = append(rmsg.Extra[sSlackAttachment], msg.Attachments)
	}

	// If we have files attached, download them (in memory) and put a pointer to it in msg.Extra.
	for i := range msg.Files {
		// keep reference in cache on which channel we added this file
		b.cache.Add(cfileDownloadChannel+msg.Files[i].ID, msg.Channel)
		if err := b.handleDownloadFile(rmsg, &msg.Files[i], false); err != nil {
			b.Log.Errorf("Could not download incoming file: %#v", err)
		}
	}
}

func (b *Bslack) handleTypingEvent(ev *slack.UserTypingEvent) (*config.Message, error) {
	if ev.User == b.actingUserID {
		return nil, ErrEventIgnored
	}
	channelInfo, err := b.channels.getChannelByID(ev.Channel)
	if err != nil {
		return nil, err
	}
	return &config.Message{
		Channel: channelInfo.Name,
		Account: b.Account,
		Event:   config.EventUserTyping,
	}, nil
}

// handleDownloadFile handles file download
func (b *Bslack) handleDownloadFile(rmsg *config.Message, file *slack.File, retry bool) error {
	if b.fileCached(file) {
		return nil
	}
	// Check that the file is neither too large nor blacklisted.
	if err := helper.HandleDownloadSize(b.Log, rmsg, file.Name, int64(file.Size), b.General); err != nil {
		b.Log.WithError(err).Infof("Skipping download of incoming file.")
		return nil
	}

	// Actually download the file.
	data, err := helper.DownloadFileAuth(file.URLPrivateDownload, "Bearer "+b.GetString(tokenConfig))
	if err != nil {
		return fmt.Errorf("download %s failed %#v", file.URLPrivateDownload, err)
	}

	if len(*data) != file.Size && !retry {
		b.Log.Debugf("Data size (%d) is not equal to size declared (%d)\n", len(*data), file.Size)
		time.Sleep(1 * time.Second)
		return b.handleDownloadFile(rmsg, file, true)
	}

	// If a comment is attached to the file(s) it is in the 'Text' field of the Slack messge event
	// and should be added as comment to only one of the files. We reset the 'Text' field to ensure
	// that the comment is not duplicated.
	comment := rmsg.Text
	rmsg.Text = ""
	helper.HandleDownloadData2(b.Log, rmsg, file.Name, file.ID, comment, file.URLPrivateDownload, data, b.General)
	return nil
}

// handleGetChannelMembers handles messages containing the GetChannelMembers event
// Sends a message to the router containing *config.ChannelMembers
func (b *Bslack) handleGetChannelMembers(rmsg *config.Message) bool {
	if rmsg.Event != config.EventGetChannelMembers {
		return false
	}

	cMembers := b.channels.getChannelMembers(b.users)

	extra := make(map[string][]any)
	extra[config.EventGetChannelMembers] = append(extra[config.EventGetChannelMembers], cMembers)
	msg := config.Message{
		Extra:   extra,
		Event:   config.EventGetChannelMembers,
		Account: b.Account,
	}

	b.Log.Debugf("sending msg to remote %#v", msg)
	b.Remote <- msg

	return true
}

// fileCached implements Matterbridge's caching logic for files
// shared via Slack.
//
// We consider that a file was cached if its ID was added in the last minute or
// it's name was registered in the last 10 seconds. This ensures that an
// identically named file but with different content will be uploaded correctly
// (the assumption is that such name collisions will not occur within the given
// timeframes).
func (b *Bslack) fileCached(file *slack.File) bool {
	if ts, ok := b.cache.Get("file" + file.ID); ok && time.Since(ts.(time.Time)) < time.Minute {
		return true
	} else if ts, ok = b.cache.Get("filename" + file.Name); ok && time.Since(ts.(time.Time)) < 10*time.Second {
		return true
	}
	return false
}
