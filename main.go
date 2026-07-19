package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ---------- Data model ----------

type UserStats struct {
	ID            int64  `bson:"_id" json:"id"`
	Username      string `bson:"username" json:"username"`
	FirstName     string `bson:"first_name" json:"first_name"`
	ContactsAdded int    `bson:"contacts_added" json:"contacts_added"`
}

// ---------- MongoDB-backed store ----------

type Store struct {
	client *mongo.Client
	coll   *mongo.Collection
}

// NewStore connects to MongoDB and returns a Store backed by the
// "users" collection in the given database.
func NewStore(uri, dbName string) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	coll := client.Database(dbName).Collection("users")

	// Helpful for /leaderboard and /eligible queries.
	_, _ = coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "contacts_added", Value: -1}},
	})

	return &Store{client: client, coll: coll}, nil
}

func (s *Store) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.client.Disconnect(ctx)
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// getOrCreate upserts a bare record for the user (if none exists yet)
// and returns their current stats.
func (s *Store) getOrCreate(u *tgbotapi.User) (*UserStats, error) {
	ctx, cancel := ctxTimeout()
	defer cancel()

	filter := bson.M{"_id": u.ID}
	update := bson.M{
		"$setOnInsert": bson.M{"contacts_added": 0},
		"$set":         bson.M{"username": u.UserName, "first_name": u.FirstName},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)

	var stat UserStats
	err := s.coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(&stat)
	if err != nil {
		return nil, err
	}
	return &stat, nil
}

// addContacts atomically increments a user's contact count by n.
func (s *Store) addContacts(u *tgbotapi.User, n int) (*UserStats, error) {
	ctx, cancel := ctxTimeout()
	defer cancel()

	filter := bson.M{"_id": u.ID}
	update := bson.M{
		"$set": bson.M{"username": u.UserName, "first_name": u.FirstName},
		"$inc": bson.M{"contacts_added": n},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)

	var stat UserStats
	err := s.coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(&stat)
	if err != nil {
		return nil, err
	}
	return &stat, nil
}

func (s *Store) eligibleUsers(minContacts int) ([]*UserStats, error) {
	ctx, cancel := ctxTimeout()
	defer cancel()

	filter := bson.M{"contacts_added": bson.M{"$gte": minContacts}}
	findOpts := options.Find().SetSort(bson.D{{Key: "contacts_added", Value: -1}})

	cur, err := s.coll.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []*UserStats
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// allUsersSorted returns every tracked user, sorted by contacts added
// (highest first) — everyone the bot has seen, regardless of whether
// they qualify for the giveaway.
func (s *Store) allUsersSorted() ([]*UserStats, error) {
	ctx, cancel := ctxTimeout()
	defer cancel()

	findOpts := options.Find().SetSort(bson.D{{Key: "contacts_added", Value: -1}})
	cur, err := s.coll.Find(ctx, bson.M{}, findOpts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []*UserStats
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- Admins allowed to run /winners ----------
// Put the numeric Telegram user IDs of your group admins here.
// You can get your own numeric ID by messaging @userinfobot.

var adminIDs = map[int64]bool{
	// 123456789: true,
}

func isAdmin(id int64) bool {
	return adminIDs[id]
}

// ---------- Main ----------

func main() {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		token="8178495226:AAHNC9MtmB7Ao5KjxHlDNw7Zb7dJ9qURECE"
	}

	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		mongoURI="mongodb://localhost:27017"
	}
	mongoDB := os.Getenv("MONGODB_DB")
	if mongoDB == "" {
		mongoDB = "giveaway_bot"
	}

	store, err := NewStore(mongoURI, mongoDB)
	if err != nil {
		log.Fatal("mongodb connection failed: ", err)
	}
	defer store.Close()
	log.Println("Connected to MongoDB, database:", mongoDB)

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal(err)
	}
	bot.Debug = false
	log.Printf("Authorized as @%s", bot.Self.UserName)

	setupCommands(bot)

	rand.Seed(time.Now().UnixNano())

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		handleUpdate(bot, store, update)
	}
}

func handleUpdate(bot *tgbotapi.BotAPI, store *Store, update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		handleCallback(bot, store, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}
	msg := update.Message

	// 1. Someone added new members to the group
	if len(msg.NewChatMembers) > 0 && msg.From != nil {
		validAdds := 0
		for _, newMember := range msg.NewChatMembers {
			if newMember.IsBot {
				continue
			}
			if newMember.ID == msg.From.ID {
				// Self-join via invite link — doesn't count as "adding" someone.
				continue
			}
			validAdds++
		}
		if validAdds > 0 {
			stat, err := store.addContacts(msg.From, validAdds)
			if err != nil {
				log.Println("addContacts error:", err)
				return
			}
			// Intentionally silent in the group — no message sent here, to
			// avoid flooding the chat every time someone adds members.
			// Users can check their running total any time via /mystats,
			// /menu, or the /leaderboard.
			log.Printf("%s added %d member(s), new total: %d", displayName(msg.From), validAdds, stat.ContactsAdded)
		}
		return
	}

	if msg.From == nil {
		return
	}

	// 2. Commands
	if !msg.IsCommand() {
		return
	}

	switch msg.Command() {

	case "start", "menu":
		m := tgbotapi.NewMessage(msg.Chat.ID, "What would you like to see?")
		m.ReplyMarkup = mainMenuKeyboard()
		bot.Send(m)

	case "mystats":
		stat, err := store.getOrCreate(msg.From)
		if err != nil {
			log.Println("getOrCreate error:", err)
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Something went wrong reading your stats, try again."))
			return
		}
		text := "Your stats:\nContacts added: " + strconv.Itoa(stat.ContactsAdded)
		bot.Send(tgbotapi.NewMessage(msg.Chat.ID, text))

	case "eligible":
		list, err := store.eligibleUsers(5)
		if err != nil {
			log.Println("eligibleUsers error:", err)
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Something went wrong reading the list, try again."))
			return
		}
		bot.Send(tgbotapi.NewMessage(msg.Chat.ID, formatEligible(list)))

	case "randomizer":
		list, err := store.eligibleUsers(5)
		if err != nil {
			log.Println("eligibleUsers error:", err)
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Something went wrong reading the list, try again."))
			return
		}
		bot.Send(randomizerMessage(msg.Chat.ID, list))

	case "leaderboard":
		list, err := store.allUsersSorted()
		if err != nil {
			log.Println("allUsersSorted error:", err)
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Something went wrong reading the list, try again."))
			return
		}
		bot.Send(tgbotapi.NewMessage(msg.Chat.ID, formatLeaderboard(list)))

	case "winners":
		if !isAdmin(msg.From.ID) {
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Only admins can draw winners."))
			return
		}
		list, err := store.eligibleUsers(5)
		if err != nil {
			log.Println("eligibleUsers error:", err)
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Something went wrong reading the list, try again."))
			return
		}
		if len(list) == 0 {
			bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "No eligible users yet (need ≥5 valid adds)."))
			return
		}
		rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		n := 3
		if len(list) < n {
			n = len(list)
		}
		winners := list[:n]
		text := "🏆 Giveaway winners:\n"
		for i, w := range winners {
			text += strconv.Itoa(i+1) + ". " + displayName2(w) + " (" + strconv.Itoa(w.ContactsAdded) + " contacts)\n"
		}
		bot.Send(tgbotapi.NewMessage(msg.Chat.ID, text))
	}
}

// ---------- Helpers ----------

// setupCommands registers the "/" command list so Telegram shows the
// native menu button (☰) next to the message box with these options.
func setupCommands(bot *tgbotapi.BotAPI) {
	commands := []tgbotapi.BotCommand{
		{Command: "menu", Description: "Show quick action buttons"},
		{Command: "mystats", Description: "Check how many contacts you've added"},
		{Command: "leaderboard", Description: "See everyone's contact counts"},
		{Command: "eligible", Description: "See who currently qualifies for the giveaway"},
		{Command: "randomizer", Description: "Get a plain copyable name list for a randomizer tool"},
		{Command: "winners", Description: "(Admins) Draw 3 random winners"},
	}
	cfg := tgbotapi.NewSetMyCommands(commands...)
	if _, err := bot.Request(cfg); err != nil {
		log.Println("failed to register bot commands:", err)
	}
}

// mainMenuKeyboard builds the tappable inline buttons sent by /menu.
func mainMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Leaderboard", "leaderboard"),
			tgbotapi.NewInlineKeyboardButtonData("🏆 Eligible", "eligible"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🙋 My stats", "mystats"),
			tgbotapi.NewInlineKeyboardButtonData("🎲 Names for randomizer", "randomizer"),
		),
	)
}

// handleCallback responds when a user taps one of the inline buttons.
func handleCallback(bot *tgbotapi.BotAPI, store *Store, cq *tgbotapi.CallbackQuery) {
	// Acknowledge the tap so Telegram stops showing the loading spinner.
	bot.Request(tgbotapi.NewCallback(cq.ID, ""))

	if cq.From == nil || cq.Message == nil {
		return
	}

	var text string
	switch cq.Data {
	case "leaderboard":
		list, err := store.allUsersSorted()
		if err != nil {
			log.Println("allUsersSorted error:", err)
			return
		}
		text = formatLeaderboard(list)
	case "eligible":
		list, err := store.eligibleUsers(5)
		if err != nil {
			log.Println("eligibleUsers error:", err)
			return
		}
		text = formatEligible(list)
	case "randomizer":
		list, err := store.eligibleUsers(5)
		if err != nil {
			log.Println("eligibleUsers error:", err)
			return
		}
		bot.Send(randomizerMessage(cq.Message.Chat.ID, list))
		return
	case "mystats":
		stat, err := store.getOrCreate(cq.From)
		if err != nil {
			log.Println("getOrCreate error:", err)
			return
		}
		text = "Your stats:\nContacts added: " + strconv.Itoa(stat.ContactsAdded)
	default:
		return
	}

	bot.Send(tgbotapi.NewMessage(cq.Message.Chat.ID, text))
}

func displayName(u *tgbotapi.User) string {
	if u.UserName != "" {
		return "@" + u.UserName
	}
	return u.FirstName
}

func displayName2(u *UserStats) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

func formatLeaderboard(list []*UserStats) string {
	if len(list) == 0 {
		return "No users tracked yet."
	}
	text := "📋 Full list — contacts added per user:\n"
	for i, u := range list {
		text += strconv.Itoa(i+1) + ". " + displayName2(u) + ": " +
			strconv.Itoa(u.ContactsAdded) + "\n"
	}
	return text
}

func formatEligible(list []*UserStats) string {
	if len(list) == 0 {
		return "No eligible users yet."
	}
	text := "Eligible users (≥5 adds):\n"
	for _, u := range list {
		text += "- " + displayName2(u) + ": " + strconv.Itoa(u.ContactsAdded) + " contacts\n"
	}
	return text
}

// formatNamesForRandomizer returns just the eligible users' names, one per
// line, with no numbering, emojis, or counts — meant to be pasted directly
// into an external randomizer/wheel-picker tool.
func formatNamesForRandomizer(list []*UserStats) string {
	if len(list) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, u := range list {
		sb.WriteString(nameForRandomizer(u))
		sb.WriteString("\n")
	}
	return sb.String()
}

// nameForRandomizer prefers the username; falls back to first name if the
// user has no username set.
func nameForRandomizer(u *UserStats) string {
	if u.Username != "" {
		return u.Username
	}
	return u.FirstName
}

// randomizerMessage builds the chat message for the "names for randomizer"
// button/command. The names are wrapped in a Markdown code block, which on
// Telegram mobile renders as a single tap-to-copy box.
func randomizerMessage(chatID int64, list []*UserStats) tgbotapi.MessageConfig {
	names := formatNamesForRandomizer(list)
	var body string
	if names == "" {
		body = "No eligible users yet (need ≥5 valid adds)."
	} else {
		body = "🎲 Tap to copy, then paste into your randomizer:\n```\n" + names + "```"
	}
	m := tgbotapi.NewMessage(chatID, body)
	m.ParseMode = "Markdown"
	return m
}
