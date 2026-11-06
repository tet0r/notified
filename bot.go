package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The optional Discord bot adds buttons to container-update notices, so an
// update can be started from Discord. Notified opens the connection to
// Discord itself, so nothing has to be reachable from the internet. Clicks
// are only accepted from Discord users on the admin's allowlist, always need
// a second confirming click, and can only act on containers already on the
// pending list (the same rule the web page follows).

// botAction is a button attached to a notification. ID is the Discord
// custom_id: "u|<integration>|<env>|<container>" to update, "i|<integration>|<name>" to ignore.
type botAction struct {
	Label string
	Style discordgo.ButtonStyle
	ID    string
}

// pendingConfirm is a click awaiting its "Yes" from the same person.
type pendingConfirm struct {
	userID    string
	kind      string // "u" or "i"
	parts     []string
	channelID string
	messageID string
	embeds    []*discordgo.MessageEmbed
	expires   time.Time
}

type discordBot struct {
	a *App

	mu        sync.Mutex
	s         *discordgo.Session
	connected bool
	user      string
	appID     string
	lastErr   string
	channels  map[string]string // webhook URL -> Discord channel id
	confirms  map[string]*pendingConfirm
}

func newDiscordBot(a *App) *discordBot {
	return &discordBot{a: a, channels: map[string]string{}, confirms: map[string]*pendingConfirm{}}
}

func (a *App) botToken() string { return openSecret(a.setting("bot_token")) }

func (a *App) botAllowed(userID string) bool {
	for _, id := range splitList(a.setting("bot_allowed_users")) {
		if id == userID {
			return true
		}
	}
	return false
}

// restart (re)connects with the saved token, or disconnects if there is none.
func (b *discordBot) restart() {
	b.mu.Lock()
	old := b.s
	b.s, b.connected, b.user, b.appID, b.lastErr = nil, false, "", "", ""
	b.channels = map[string]string{}
	b.mu.Unlock()
	if old != nil {
		old.Close()
	}

	token := b.a.botToken()
	if token == "" {
		return
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		b.setErr(err)
		return
	}
	s.Identify.Intents = discordgo.IntentsGuilds // no message content needed: buttons only
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		b.mu.Lock()
		b.connected, b.lastErr = true, ""
		b.user = r.User.Username
		b.appID = r.User.ID
		if r.Application != nil && r.Application.ID != "" {
			b.appID = r.Application.ID
		}
		b.mu.Unlock()
		log.Printf("discord bot connected as %s", r.User.Username)
	})
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { b.setConnected(true) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { b.setConnected(false) })
	s.AddHandler(b.onInteraction)
	if err := s.Open(); err != nil {
		b.setErr(fmt.Errorf("could not connect to Discord: %w", err))
		return
	}
	b.mu.Lock()
	b.s = s
	b.mu.Unlock()
}

func (b *discordBot) close() {
	b.mu.Lock()
	s := b.s
	b.s, b.connected = nil, false
	b.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (b *discordBot) setErr(err error) {
	b.mu.Lock()
	b.lastErr = err.Error()
	b.connected = false
	b.mu.Unlock()
	log.Printf("discord bot: %v", err)
}

func (b *discordBot) setConnected(v bool) {
	b.mu.Lock()
	b.connected = v
	b.mu.Unlock()
}

func (b *discordBot) session() *discordgo.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.connected {
		return nil
	}
	return b.s
}

type botStatus struct {
	TokenSet     bool   `json:"token_set"`
	AllowedUsers string `json:"allowed_users"`
	Connected    bool   `json:"connected"`
	User         string `json:"user,omitempty"`
	InviteURL    string `json:"invite_url,omitempty"`
	Error        string `json:"error,omitempty"`
}

// botPermissions: view channels, send messages, embed links, read message history.
const botPermissions = 1024 + 2048 + 16384 + 65536

func (b *discordBot) status() botStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := botStatus{
		TokenSet:     b.a.setting("bot_token") != "",
		AllowedUsers: b.a.setting("bot_allowed_users"),
		Connected:    b.connected,
		User:         b.user,
		Error:        b.lastErr,
	}
	if b.appID != "" {
		st.InviteURL = fmt.Sprintf("https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d", b.appID, botPermissions)
	}
	return st
}

// channelFor finds the Discord channel a Notified channel's webhook posts to.
// Discord reports it for a webhook URL without any further credentials.
func (b *discordBot) channelFor(ctx context.Context, webhookURL string) (string, error) {
	b.mu.Lock()
	id, ok := b.channels[webhookURL]
	b.mu.Unlock()
	if ok {
		return id, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webhookURL, nil)
	if err != nil {
		return "", errors.New("invalid webhook URL")
	}
	resp, err := b.a.http.Do(req)
	if err != nil {
		return "", errors.New("could not reach Discord: " + unwrapNetErr(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var info struct {
		ChannelID string `json:"channel_id"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &info) != nil || info.ChannelID == "" {
		return "", fmt.Errorf("could not look up the channel behind this webhook (HTTP %d)", resp.StatusCode)
	}
	b.mu.Lock()
	b.channels[webhookURL] = info.ChannelID
	b.mu.Unlock()
	return info.ChannelID, nil
}

func toDiscordEmbed(e Embed) *discordgo.MessageEmbed {
	data, _ := json.Marshal(e) // unexported fields (attachments, buttons) are left out
	var out discordgo.MessageEmbed
	json.Unmarshal(data, &out)
	return &out
}

func buttonsRow(actions []botAction) []discordgo.MessageComponent {
	var btns []discordgo.MessageComponent
	for _, a := range actions {
		btns = append(btns, discordgo.Button{Label: a.Label, Style: a.Style, CustomID: a.ID})
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: btns}}
}

// sendWithButtons posts an embed with buttons as the bot, in the channel
// behind the given webhook. The caller falls back to the webhook (without
// buttons) if this fails.
func (b *discordBot) sendWithButtons(ctx context.Context, webhookURL string, e Embed) error {
	s := b.session()
	if s == nil {
		return errors.New("the bot is not connected")
	}
	channelID, err := b.channelFor(ctx, webhookURL)
	if err != nil {
		return err
	}
	_, err = s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{toDiscordEmbed(e)},
		Components:      buttonsRow(e.actions),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		err = fmt.Errorf("the bot could not post in that channel (is it invited, with permission to send messages?): %w", err)
		b.mu.Lock()
		b.lastErr = err.Error()
		b.mu.Unlock()
	}
	return err
}

// sendTest posts a message with a test button to the given webhook's channel.
func (b *discordBot) sendTest(ctx context.Context, webhookURL string) error {
	e := Embed{
		Title:       "Notified bot test",
		Description: "Press the button. It tells you your Discord user ID and whether you are allowed to use update buttons.",
		Color:       colorBlurple,
	}
	e.actions = []botAction{{Label: "Press me", Style: discordgo.PrimaryButton, ID: "t"}}
	return b.sendWithButtons(ctx, webhookURL, e)
}

// ---- clicks ----

func interactionUser(ic *discordgo.InteractionCreate) *discordgo.User {
	if ic.Member != nil && ic.Member.User != nil {
		return ic.Member.User
	}
	return ic.User
}

func (b *discordBot) reply(s *discordgo.Session, ic *discordgo.InteractionCreate, text string, components []discordgo.MessageComponent) {
	err := s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:         text,
			Flags:           discordgo.MessageFlagsEphemeral, // only the person who clicked sees it
			Components:      components,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	})
	if err != nil {
		log.Printf("discord bot: reply failed: %v", err)
	}
}

// updateReply replaces the text of the ephemeral message a button sits on.
func (b *discordBot) updateReply(s *discordgo.Session, ic *discordgo.InteractionCreate, text string) {
	err := s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: text, Components: []discordgo.MessageComponent{}},
	})
	if err != nil {
		log.Printf("discord bot: reply failed: %v", err)
	}
}

func notAllowedText(u *discordgo.User) string {
	return fmt.Sprintf("You're not allowed to use these buttons. To allow it, add your Discord user ID `%s` in Notified → Settings → Discord bot.", u.ID)
}

func (b *discordBot) onInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic.Type != discordgo.InteractionMessageComponent {
		return
	}
	u := interactionUser(ic)
	if u == nil {
		return
	}
	id := ic.MessageComponentData().CustomID
	allowed := b.a.botAllowed(u.ID)

	switch {
	case id == "t":
		if allowed {
			b.reply(s, ic, fmt.Sprintf("It works. Your Discord user ID is `%s` and you're allowed to use update buttons.", u.ID), nil)
		} else {
			b.reply(s, ic, fmt.Sprintf("It works. Your Discord user ID is `%s`. Add it in Notified → Settings → Discord bot to allow update buttons.", u.ID), nil)
		}
	case id == "no":
		b.updateReply(s, ic, "Cancelled. Nothing was changed.")
	case strings.HasPrefix(id, "ok:"):
		b.confirmed(s, ic, u, strings.TrimPrefix(id, "ok:"))
	case strings.HasPrefix(id, "u|"), strings.HasPrefix(id, "i|"):
		if !allowed {
			b.reply(s, ic, notAllowedText(u), nil)
			return
		}
		b.askConfirm(s, ic, u, id)
	}
}

// askConfirm answers an Update or Ignore click with a private "are you sure".
func (b *discordBot) askConfirm(s *discordgo.Session, ic *discordgo.InteractionCreate, u *discordgo.User, id string) {
	parts := strings.Split(id, "|")
	intID, _ := strconv.ParseInt(parts[1], 10, 64)
	in, err := b.a.getIntegration(intID)
	if err != nil || !isUpdateType(in.Type) {
		b.reply(s, ic, "That integration no longer exists in Notified.", nil)
		b.finishMessage(s, ic.Message.ChannelID, ic.Message.ID, ic.Message.Embeds, colorGrey, "Status", "No longer available")
		return
	}
	var question string
	switch parts[0] {
	case "u":
		if len(parts) != 4 {
			return
		}
		envID, _ := strconv.ParseInt(parts[2], 10, 64)
		item, ok := b.a.findPending(in.ID, envID, parts[3])
		if !ok {
			b.reply(s, ic, "That update is no longer pending: it was already applied, ignored, or the container changed. Run **Check now** in Notified to refresh.", nil)
			b.finishMessage(s, ic.Message.ChannelID, ic.Message.ID, ic.Message.Embeds, colorGrey, "Status", "No longer pending")
			return
		}
		question = fmt.Sprintf("Update **%s** now? Notified will pull `%s` and recreate the container with the same settings. It will be briefly unavailable.", item.Name, item.Image)
	case "i":
		if len(parts) != 3 {
			return
		}
		question = fmt.Sprintf("Ignore **%s**? Notified will stop checking it for updates until you unignore it in the integration's settings.", parts[2])
	}

	token := randomToken(8)
	b.mu.Lock()
	now := time.Now()
	for k, c := range b.confirms {
		if now.After(c.expires) {
			delete(b.confirms, k)
		}
	}
	b.confirms[token] = &pendingConfirm{
		userID: u.ID, kind: parts[0], parts: parts,
		channelID: ic.Message.ChannelID, messageID: ic.Message.ID, embeds: ic.Message.Embeds,
		expires: now.Add(10 * time.Minute),
	}
	b.mu.Unlock()

	label := "Yes, update"
	style := discordgo.SuccessButton
	if parts[0] == "i" {
		label, style = "Yes, ignore", discordgo.DangerButton
	}
	b.reply(s, ic, question, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: label, Style: style, CustomID: "ok:" + token},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "no"},
	}}})
}

// confirmed carries out an action after its second click.
func (b *discordBot) confirmed(s *discordgo.Session, ic *discordgo.InteractionCreate, u *discordgo.User, token string) {
	b.mu.Lock()
	c := b.confirms[token]
	delete(b.confirms, token)
	b.mu.Unlock()
	switch {
	case c == nil || time.Now().After(c.expires):
		b.updateReply(s, ic, "That confirmation has expired. Press the button on the notification again.")
		return
	case c.userID != u.ID || !b.a.botAllowed(u.ID):
		b.updateReply(s, ic, notAllowedText(u))
		return
	}
	intID, _ := strconv.ParseInt(c.parts[1], 10, 64)
	actor := u.Username + " (Discord)"

	if c.kind == "i" {
		in, err := b.a.getIntegration(intID)
		if err == nil {
			err = b.a.setIgnored(in, c.parts[2], true)
		}
		if err != nil {
			b.updateReply(s, ic, "Could not ignore it: "+err.Error())
			return
		}
		b.updateReply(s, ic, fmt.Sprintf("**%s** is now ignored.", c.parts[2]))
		b.finishMessage(s, c.channelID, c.messageID, c.embeds, colorGrey, "Ignored", "by "+u.Mention())
		return
	}

	// Answer within Discord's three seconds, then do the slow part.
	b.updateReply(s, ic, "Updating… this can take a few minutes while the image downloads.")
	envID, _ := strconv.ParseInt(c.parts[2], 10, 64)
	go func() {
		item, err := b.a.applyUpdate(intID, envID, c.parts[3], actor)
		var msg string
		switch {
		case errors.Is(err, errNotPending):
			msg = "That update is no longer pending, so nothing was changed."
			b.finishMessage(s, c.channelID, c.messageID, c.embeds, colorGrey, "Status", "No longer pending")
		case err != nil:
			msg = fmt.Sprintf("The update of **%s** failed: %s", item.Name, trunc(err.Error(), 1500))
			b.noteOnMessage(s, c.channelID, c.messageID, c.embeds, "Last attempt failed", trunc(err.Error(), 300))
		default:
			msg = fmt.Sprintf("**%s** was updated.", item.Name)
			b.finishMessage(s, c.channelID, c.messageID, c.embeds, colorGreen, "Updated", "by "+u.Mention())
		}
		content := msg
		if _, err := s.InteractionResponseEdit(ic.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			log.Printf("discord bot: could not report the result: %v", err)
		}
	}()
}

// finishMessage marks the original notice as handled and removes its buttons.
func (b *discordBot) finishMessage(s *discordgo.Session, channelID, messageID string, embeds []*discordgo.MessageEmbed, color int, field, value string) {
	if len(embeds) == 0 {
		return
	}
	e := *embeds[0]
	e.Color = color
	e.Fields = append(append([]*discordgo.MessageEmbedField{}, e.Fields...), &discordgo.MessageEmbedField{Name: field, Value: value, Inline: true})
	none := []discordgo.MessageComponent{}
	list := []*discordgo.MessageEmbed{&e}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID, Embeds: &list, Components: &none,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}); err != nil {
		log.Printf("discord bot: could not update the notice: %v", err)
	}
}

// noteOnMessage adds a line to the original notice and keeps its buttons, so
// a failed update can be retried.
func (b *discordBot) noteOnMessage(s *discordgo.Session, channelID, messageID string, embeds []*discordgo.MessageEmbed, field, value string) {
	if len(embeds) == 0 {
		return
	}
	e := *embeds[0]
	var fields []*discordgo.MessageEmbedField
	for _, f := range e.Fields {
		if f.Name != field {
			fields = append(fields, f)
		}
	}
	e.Fields = append(fields, &discordgo.MessageEmbedField{Name: field, Value: value})
	list := []*discordgo.MessageEmbed{&e}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{Channel: channelID, ID: messageID, Embeds: &list}); err != nil {
		log.Printf("discord bot: could not update the notice: %v", err)
	}
}

// updateActions builds the buttons for a container-update notice.
func updateActions(integrationID int64, it pendingUpdate) []botAction {
	acts := []botAction{{Label: "Update", Style: discordgo.PrimaryButton, ID: fmt.Sprintf("u|%d|%d|%s", integrationID, it.EnvID, it.ContainerID)}}
	ignore := fmt.Sprintf("i|%d|%s", integrationID, it.Name)
	if len(ignore) <= 100 && !strings.Contains(it.Name, "|") {
		acts = append(acts, botAction{Label: "Ignore", Style: discordgo.SecondaryButton, ID: ignore})
	}
	if len(acts[0].ID) > 100 { // Discord's limit for a button id
		return acts[1:]
	}
	return acts
}

// ---- HTTP API for the settings page ----

func (a *App) handleBotGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.bot.status())
}

func (a *App) handleBotSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token        string `json:"token"`
		ClearToken   bool   `json:"clear_token"`
		AllowedUsers string `json:"allowed_users"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var ids []string
	for _, id := range splitList(body.AllowedUsers) {
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a Discord user ID (they are long numbers, like 123456789012345678)", id))
			return
		}
		ids = append(ids, id)
	}
	a.setSetting("bot_allowed_users", strings.Join(ids, ", "))
	token := strings.TrimSpace(body.Token)
	changed := false
	switch {
	case body.ClearToken:
		a.setSetting("bot_token", "")
		changed = true
	case token != "":
		a.setSetting("bot_token", sealSecret(token))
		changed = true
	}
	if changed {
		a.bot.restart()
		// Give the connection a moment so the page can show the result.
		for i := 0; i < 20 && a.bot.session() == nil && a.bot.status().Error == "" && a.botToken() != ""; i++ {
			time.Sleep(250 * time.Millisecond)
		}
	}
	writeJSON(w, http.StatusOK, a.bot.status())
}

func (a *App) handleBotTest(w http.ResponseWriter, r *http.Request) {
	chID := a.overviewChannel()
	ch, err := a.getChannel(chID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "add a Discord channel first")
		return
	}
	if err := a.bot.sendTest(r.Context(), ch.WebhookURL); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"channel": ch.Name})
}
