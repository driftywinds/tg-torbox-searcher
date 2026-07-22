package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"torbox-tg-bot/internal/store"
	"torbox-tg-bot/internal/torbox"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	tb       *torbox.Client
	users    *store.UserStore
	sessions *sessionStore
}

func New(api *tgbotapi.BotAPI, tb *torbox.Client, users *store.UserStore) *Bot {
	return &Bot{
		api:      api,
		tb:       tb,
		users:    users,
		sessions: newSessionStore(),
	}
}

// Run starts the long-polling update loop. It blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}
			// Handle each message concurrently so a slow search or gofile
			// poll for one user doesn't block replies to another.
			go b.handleMessage(update.Message)
		}
	}
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = false
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send message to %d failed: %v", chatID, err)
	}
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("recovered from panic handling message: %v", r)
			b.send(msg.Chat.ID, "Something went wrong handling that. Please try again.")
		}
	}()

	userID := msg.From.ID
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	if text == "" {
		return
	}

	// Commands are handled first, since /adduser etc. should work even for
	// people who aren't authorized yet (the admin check happens inside).
	if strings.HasPrefix(text, "/") {
		b.handleCommand(userID, chatID, text)
		return
	}

	if !b.users.IsAuthorized(userID) {
		b.send(chatID, "You're not authorized to use this bot. Ask the admin to add you with /adduser.")
		return
	}

	// A bare number replies to the last search's numbered list.
	if idx, err := strconv.Atoi(text); err == nil {
		b.handleSelection(chatID, idx)
		return
	}

	b.doSearch(chatID, text)
}

func (b *Bot) handleCommand(userID, chatID int64, text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "/start", "/help":
		if !b.users.IsAuthorized(userID) {
			b.send(chatID, "You're not authorized to use this bot. Ask the admin to add you.")
			return
		}
		b.send(chatID, helpText(b.users.IsAdmin(userID)))

	case "/adduser":
		if !b.users.IsAdmin(userID) {
			b.send(chatID, "Only the admin can do that.")
			return
		}
		if len(args) != 1 {
			b.send(chatID, "Usage: /adduser <telegram_user_id>")
			return
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			b.send(chatID, "That doesn't look like a valid numeric Telegram user id.")
			return
		}
		added, err := b.users.Add(id)
		if err != nil {
			b.send(chatID, "Couldn't add that user: "+err.Error())
			return
		}
		if added {
			b.send(chatID, fmt.Sprintf("✅ User %d can now use the bot.", id))
		} else {
			b.send(chatID, fmt.Sprintf("User %d was already authorized.", id))
		}

	case "/removeuser":
		if !b.users.IsAdmin(userID) {
			b.send(chatID, "Only the admin can do that.")
			return
		}
		if len(args) != 1 {
			b.send(chatID, "Usage: /removeuser <telegram_user_id>")
			return
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			b.send(chatID, "That doesn't look like a valid numeric Telegram user id.")
			return
		}
		removed, err := b.users.Remove(id)
		if err != nil {
			b.send(chatID, "Couldn't remove that user: "+err.Error())
			return
		}
		if removed {
			b.send(chatID, fmt.Sprintf("🗑️ User %d has been removed.", id))
		} else {
			b.send(chatID, fmt.Sprintf("User %d wasn't authorized to begin with.", id))
		}

	case "/cancel":
		if !b.users.IsAuthorized(userID) {
			b.send(chatID, "You're not authorized to use this bot.")
			return
		}
		b.sessions.del(chatID)
		b.send(chatID, "❌ Cancelled. Send me a search query to start over.")

	case "/listusers":
		if !b.users.IsAdmin(userID) {
			b.send(chatID, "Only the admin can do that.")
			return
		}
		ids := b.users.List()
		if len(ids) == 0 {
			b.send(chatID, "No additional authorized users (besides you, the admin).")
			return
		}
		var sb strings.Builder
		sb.WriteString("Authorized users:\n")
		for _, id := range ids {
			sb.WriteString(fmt.Sprintf("- %d\n", id))
		}
		b.send(chatID, sb.String())

	default:
		if b.users.IsAuthorized(userID) {
			b.send(chatID, "Unknown command. Send /help for usage.")
		}
	}
}

func helpText(isAdmin bool) string {
	base := "" +
		"Send me any text to search TorBox.\n\n" +
		"I'll reply with a numbered list (name / size / cached status / private or public tracker). " +
		"Just reply with the number of the one you want.\n\n" +
		"- If it's already cached, I'll start it on TorBox and then upload it to GoFile, then send you the link.\n" +
		"- If it's not cached yet, I'll start the download on TorBox. Reply with the same number again later to check on it and get your GoFile link once it's ready.\n\n" +
		"Use /cancel at any time to reset and start a new search."
	if isAdmin {
		base += "\n\nAdmin commands:\n" +
			"/adduser <telegram_user_id>\n" +
			"/removeuser <telegram_user_id>\n" +
			"/listusers"
	}
	return base
}

// ---- search -----------------------------------------------------------------

func (b *Bot) doSearch(chatID int64, query string) {
	b.send(chatID, fmt.Sprintf("🔎 Searching TorBox for \"%s\"...", query))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	results, err := b.tb.Search(ctx, query)
	if err != nil {
		log.Printf("search error: %v", err)
		b.send(chatID, "Search failed: "+err.Error())
		return
	}
	if len(results) == 0 {
		b.send(chatID, "No results found.")
		return
	}

	const maxResults = 25
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	sess := newSession(query, results)
	b.sessions.set(chatID, sess)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d result(s) for \"%s\":\n\n", len(results), query))
	for i, r := range results {
		cachedStr := "⏳ not cached"
		if r.Cached {
			cachedStr = "✅ cached"
		}
		privacyStr := "🌐 public"
		if r.Private {
			privacyStr = "🔒 private"
		}
		tracker := r.Tracker
		if tracker == "" {
			tracker = "unknown source"
		}
		sb.WriteString(fmt.Sprintf(
			"%d. %s\n   %s | %s | %s | %s | seeders: %d\n\n",
			i+1, r.Name, humanSize(r.Size), cachedStr, privacyStr, tracker, r.Seeders,
		))
	}
	sb.WriteString("Reply with a number to fetch that one.")

	b.send(chatID, sb.String())
}

// ---- selection / download / gofile upload -----------------------------------

func (b *Bot) handleSelection(chatID int64, idx int) {
	sess := b.sessions.get(chatID)
	if sess == nil {
		b.send(chatID, "You don't have an active search. Send a search query first.")
		return
	}

	sess.mu.Lock()
	if idx < 1 || idx > len(sess.Results) {
		sess.mu.Unlock()
		b.send(chatID, fmt.Sprintf("Please reply with a number between 1 and %d.", len(sess.Results)))
		return
	}
	sel, exists := sess.Selections[idx]
	if !exists {
		sel = &selection{Result: sess.Results[idx-1], Stage: stageNew}
		sess.Selections[idx] = sel
	}
	stage := sel.Stage
	sess.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch stage {
	case stageNew:
		b.startSelection(ctx, chatID, idx, sess, sel)
	case stageDownloading:
		b.checkDownloadProgress(ctx, chatID, idx, sess, sel)
	case stageUploading:
		b.send(chatID, "Still uploading that one to GoFile - hang tight, I'll send the link as soon as it's ready.")
	case stageDone:
		b.send(chatID, fmt.Sprintf("✅ %s\n%s\n\nGoFile link: %s", sel.Result.Name, humanSize(sel.Result.Size), sel.GofileURL))
	case stageFailed:
		b.send(chatID, fmt.Sprintf("That one failed earlier: %s\nReply with the number again to retry.", sel.LastError))
		sess.mu.Lock()
		sel.Stage = stageNew
		sess.mu.Unlock()
	}
}

func (b *Bot) startSelection(ctx context.Context, chatID int64, idx int, sess *session, sel *selection) {
	r := sel.Result
	torrentID, hash, err := b.tb.CreateTorrentFromMagnet(ctx, r.Magnet, r.Cached)
	if err != nil {
		// If it's already on the account, try to find it instead of failing outright.
		if hash == "" {
			hash = r.Hash
		}
		if info, findErr := b.tb.FindTorrentByHash(ctx, hash); findErr == nil {
			torrentID = info.ID
		} else {
			log.Printf("createtorrent error for chat %d: %v", chatID, err)
			b.markFailed(sess, idx, err.Error())
			b.send(chatID, "Couldn't start that download: "+err.Error())
			return
		}
	}

	sess.mu.Lock()
	sel.TorrentID = torrentID
	sess.mu.Unlock()

	if r.Cached {
		sess.mu.Lock()
		sel.Stage = stageUploading
		sess.mu.Unlock()
		b.send(chatID, fmt.Sprintf("📦 \"%s\" is cached - starting it on TorBox and uploading to GoFile now. This can take a little while for larger files, I'll message you when it's ready.", r.Name))
		go b.runGofileFlow(chatID, idx, sess, sel)
		return
	}

	sess.mu.Lock()
	sel.Stage = stageDownloading
	sess.mu.Unlock()
	b.send(chatID, fmt.Sprintf(
		"⬇️ \"%s\" wasn't cached, so I've started downloading it on TorBox.\nReply with %d again in a bit to check progress and grab your GoFile link once it's ready.",
		r.Name, idx,
	))
}

func (b *Bot) checkDownloadProgress(ctx context.Context, chatID int64, idx int, sess *session, sel *selection) {
	info, err := b.tb.GetTorrentByID(ctx, sel.TorrentID)
	if err != nil {
		b.send(chatID, "Couldn't check that download's status: "+err.Error())
		return
	}

	if info.DownloadFinished || info.Cached {
		sess.mu.Lock()
		sel.Stage = stageUploading
		sess.mu.Unlock()
		b.send(chatID, fmt.Sprintf("✅ \"%s\" finished downloading on TorBox. Uploading to GoFile now, I'll message you when it's ready.", sel.Result.Name))
		go b.runGofileFlow(chatID, idx, sess, sel)
		return
	}

	pct := info.Progress * 100
	b.send(chatID, fmt.Sprintf(
		"⬇️ Still downloading \"%s\": %.1f%% (%s). State: %s. Try again in a bit.",
		sel.Result.Name, pct, humanSize(info.DownloadSpeed)+"/s", info.DownloadState,
	))
}

func (b *Bot) runGofileFlow(chatID int64, idx int, sess *session, sel *selection) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	jobID, err := b.tb.CreateGofileJob(ctx, sel.TorrentID)
	if err != nil {
		log.Printf("gofile job creation failed for chat %d: %v", chatID, err)
		b.markFailed(sess, idx, err.Error())
		b.send(chatID, "Couldn't start the GoFile upload: "+err.Error()+"\n(Note: this integration endpoint is a best-effort implementation - see the bot's README.)")
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.markFailed(sess, idx, "timed out waiting for GoFile upload")
			b.send(chatID, "Gave up waiting for the GoFile upload to finish (timed out). Reply with the number again to retry checking.")
			return
		case <-ticker.C:
			status, err := b.tb.GetJobStatus(ctx, jobID)
			if err != nil {
				log.Printf("gofile job status check failed for chat %d: %v", chatID, err)
				continue // transient errors shouldn't kill the whole poll loop
			}
			if status.IsFailed() {
				msg := status.Detail
				if msg == "" {
					msg = status.Status
				}
				b.markFailed(sess, idx, msg)
				b.send(chatID, "The GoFile upload failed: "+msg)
				return
			}
			if status.IsDone() {
				sess.mu.Lock()
				sel.Stage = stageDone
				sel.GofileURL = status.DownloadURL
				sess.mu.Unlock()
				b.send(chatID, fmt.Sprintf(
					"🎉 Done!\n%s\n%s\n\nGoFile link: %s",
					sel.Result.Name, humanSize(sel.Result.Size), status.DownloadURL,
				))
				return
			}
		}
	}
}

func (b *Bot) markFailed(sess *session, idx int, reason string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sel, ok := sess.Selections[idx]; ok {
		sel.Stage = stageFailed
		sel.LastError = reason
	}
}
