package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var db *pgxpool.Pool
var aiClient openai.Client
var ollamaClient openai.Client

const (
	StateIdle    = "idle"
	StateWaiting = "waiting"
)

type userRow struct {
	State         string
	Mode          string
	RequestCount  int
	SelectedModel string
	Seen          bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := godotenv.Load(); err != nil {
		log.Fatal("ENV")
	}
	var err error
	db, err = pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
		return
	}
	defer db.Close()

	if err := initDB(ctx); err != nil {
		log.Fatal("initDB error - ", err)
		return
	}

	b, err := bot.New(
		os.Getenv("TELEGRAM_API_KEY"),
		bot.WithDefaultHandler(handler),
		bot.WithCheckInitTimeout(5*time.Second),
	)

	if err != nil {
		panic(err)
	}

	aiClient = openai.NewClient(
		option.WithAPIKey(os.Getenv("OPEN_AI_API_KEY")),
	)

	ollamaClient = openai.NewClient(
		option.WithAPIKey("ollama"),
		option.WithBaseURL("http://ollama:11434/v1/"),
	)

	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeExact, startHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/clear", bot.MatchTypeExact, clearHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/clear_requests", bot.MatchTypeExact, clearRequestsHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/text", bot.MatchTypeExact, textModeHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/image", bot.MatchTypeExact, imageModeHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/model", bot.MatchTypeExact, modelChoicesHandler)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "model:", bot.MatchTypePrefix, modelsSelectHandler)

	user, _ := b.GetMe(ctx)
	fmt.Printf("Bot name: %#v\n", user.Username)

	_, err = b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Запуск бота"},
			{Command: "text", Description: "Текстовый режим"},
			{Command: "model", Description: "Выбрать модель"},
			{Command: "image", Description: "Режим генерации изображений"},
			{Command: "clear", Description: "Очистить историю"},
		},
	})
	if err != nil {
		log.Println("SetMyCommands", err)
	}

	b.Start(ctx)
}

func initDB(ctx context.Context) error {
	_, err := db.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS users (
			user_id       BIGINT PRIMARY KEY,
			state         TEXT NOT NULL DEFAULT 'idle',
			mode          TEXT NOT NULL DEFAULT 'text',
			request_count INT  NOT NULL DEFAULT 0,
			selected_model TEXT NOT NULL DEFAULT 'gemma3:1b',
			seen          BOOLEAN NOT NULL DEFAULT false,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS message_history (
			id         SERIAL PRIMARY KEY,
			user_id    BIGINT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now());`,
	)
	return err
}

func ensureUser(ctx context.Context, UserID int64) error {
	_, err := db.Exec(ctx, `INSERT INTO users(user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, UserID)
	return err
}

func getUser(ctx context.Context, UserID int64) (userRow, error) {
	var u userRow
	err := db.QueryRow(ctx, `
		SELECT state, mode, request_count, selected_model, seen
		FROM users WHERE user_id = $1
	`, UserID).Scan(&u.State, &u.Mode, &u.RequestCount, &u.SelectedModel, &u.Seen)
	return u, err
}

func setUserState(ctx context.Context, UserID int64, state string) error {
	_, err := db.Exec(ctx, `UPDATE users SET state=$1 WHERE user_id=$2`, state, UserID)
	return err
}

func setUserMode(ctx context.Context, UserID int64, mode string) error {
	_, err := db.Exec(ctx, `UPDATE users SET mode=$1 WHERE user_id=$2`, mode, UserID)
	return err
}

func setUserModel(ctx context.Context, UserID int64, model string) error {
	_, err := db.Exec(ctx, `UPDATE users SET selected_model=$1 WHERE user_id=$2`, model, UserID)
	return err
}

func incrementRequestCount(ctx context.Context, UserID int64) error {
	_, err := db.Exec(ctx, `UPDATE users SET request_count = request_count + 1 WHERE user_id=$1`, UserID)
	return err
}

func resetRequestCount(ctx context.Context, UserID int64) error {
	_, err := db.Exec(ctx, `UPDATE users SET request_count=0 WHERE user_id=$1`, UserID)
	return err
}

func markSeen(ctx context.Context, UserID int64) (alreadySeen bool, err error) {
	var seen bool
	err = db.QueryRow(ctx, `
		UPDATE users SET seen=true WHERE user_id=$1 RETURNING (seen AND $2)
	`, UserID, true).Scan(&seen)
	// easier: just check before update
	return seen, err
}

func appendHistory(ctx context.Context, UserID int64, role, content string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO message_history (user_id, role, content) VALUES ($1,$2,$3)
	`, UserID, role, content)
	return err
}

func getHistory(ctx context.Context, UserID int64) ([]openai.ChatCompletionMessageParamUnion, error) {
	rows, err := db.Query(ctx, `
		SELECT role, content FROM message_history
		WHERE user_id=$1 ORDER BY created_at ASC
	`, UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []openai.ChatCompletionMessageParamUnion
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		switch role {
		case "user":
			history = append(history, openai.UserMessage(content))
		case "assistant":
			history = append(history, openai.AssistantMessage(content))
		}
	}
	return history, nil
}

func clearHistory(ctx context.Context, UserID int64) error {
	_, err := db.Exec(ctx, `DELETE FROM message_history WHERE user_id=$1`, UserID)
	return err
}

func startHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserID := update.Message.From.ID
	UserName := update.Message.From.Username
	UserFirstName := update.Message.From.FirstName
	Premium := update.Message.From.IsPremium

	if err := ensureUser(ctx, UserID); err != nil {
		log.Println("ensureUser:", err)
		return
	}

	user, err := getUser(ctx, UserID)
	if err != nil {
		log.Println("getUser:", err)
		return
	}

	if !user.Seen {
		fmt.Printf("New user:\n id: %d\n Name: @%s\n Premium: %t\n", UserID, UserName, Premium)
		fmt.Printf("Начало пользования в %s\n", time.Now())
		db.Exec(ctx, `UPDATE users SET seen=true WHERE user_id=$1`, UserID)
	}

	setUserState(ctx, UserID, StateWaiting)
	setUserMode(ctx, UserID, "text")

	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("Привет, %s! 👋\nДобро пожаловать!\nПо умолчанию стоит режим генерации текста, чтобы переключиться на режим генерации изображений напиши /image, а чтобы вернуться, напишите /text\nЧтобы сбросить историю в текстовом генераторе напишите /clear_requests", UserFirstName),
	})
	if err != nil {
		log.Println(err)
		return
	}

	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Напиши свой запрос одним сообщением",
	})
	if err != nil {
		log.Println(err)
		return
	}
}

func handler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	UserID := update.Message.From.ID

	if err := ensureUser(ctx, UserID); err != nil {
		log.Println("ensureUser:", err)
		return
	}

	user, err := getUser(ctx, UserID)
	if err != nil {
		log.Println("getUser:", err)
		return
	}

	if user.State != StateWaiting {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Напиши /start чтобы начать\n/text — текстовый режим\n/image — режим изображений",
		})
		if err != nil {
			log.Println(err)
			return
		}
		return
	}

	if user.RequestCount >= 5 {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "❌ Вы исчерпали лимит в 5 запросов.\nНапишите @Longin_khibovskiy для покупки 😂",
		})
		if err != nil {
			log.Println(err)
			return
		}
		fmt.Printf("Лимит достинут в %s\n", time.Now())
		return
	}

	fmt.Println(UserID, " - ", user.RequestCount)

	question := update.Message.Text

	switch user.Mode {
	case "text":
		handleText(ctx, b, update, UserID, user.SelectedModel, question)
	case "image":
		handleImage(ctx, b, update, UserID, user.RequestCount, question)
	}
}

func clearHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserID := update.Message.From.ID
	if err := clearHistory(ctx, UserID); err != nil {
		log.Println("clearHistory:", err)
	}

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Ваша история очищена",
	})
	if err != nil {
		log.Println(err)
		return
	}
}

func clearRequestsHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if err := resetRequestCount(ctx, update.Message.From.ID); err != nil {
		log.Println("resetRequestCount:", err)
	}

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Лимит запросов сброшен. Доступно снова 5 запросов.",
	})
	if err != nil {
		log.Println(err)
		return
	}
}

func textModeHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserID := update.Message.From.ID
	ensureUser(ctx, UserID)
	setUserMode(ctx, UserID, "text")
	setUserState(ctx, UserID, StateWaiting)

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "💬 Режим текст. Модель Ollama\nЧтобы сбросить историю напишите /clear_requests\nНапиши свой вопрос:",
	})
	if err != nil {
		log.Println(err)
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Произошла ошибка, попробуйте чуть позже🙏",
		})
		if err != nil {
			log.Println(err)
			return
		}
		return
	}
}

func imageModeHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserID := update.Message.From.ID
	ensureUser(ctx, UserID)
	setUserMode(ctx, UserID, "image")
	setUserState(ctx, UserID, StateWaiting)

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "🎨 Режим изображения (В день доступно 5 бесплатных генераций). Модель open ai\nНапиши что хочешь изобразить:",
	})
	if err != nil {
		log.Println(err)
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Произошла ошибка, попробуйте чуть позже🙏",
		})
		if err != nil {
			log.Println(err)
			return
		}
		return

	}
}

func handleText(ctx context.Context, b *bot.Bot, update *models.Update, UserID int64, modelName string, question string) {
	history, err := getHistory(ctx, UserID)
	if err != nil {
		log.Println("getHistory:", err)
	}
	history = append(history, openai.UserMessage(question))

	message, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Думает...",
	})
	if err != nil {
		log.Println(err)
		return
	}

	_, err = b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: update.Message.Chat.ID,
		Action: models.ChatActionTyping,
	})
	if err != nil {
		log.Println(err)
		return
	}

	response, err := ollamaClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     openai.ChatModel(modelName),
		MaxTokens: openai.Int(256),
		Messages: append([]openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("Отвечай максимум 150 символами. В ответе должна быть вся необходимая информация. Не пиши про символы"),
		}, history...),
	})
	if err != nil {
		log.Println("handleText - send question", err)
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "❌ Ошибка, попробуйте еще разочек",
		})
		if err != nil {
			log.Println(err)
			return
		}
		return
	}

	_, err = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    update.Message.Chat.ID,
		MessageID: message.ID,
	})
	if err != nil {
		log.Println(err)
		return
	}

	answer := response.Choices[0].Message.Content
	if err := appendHistory(ctx, UserID, "user", question); err != nil {
		log.Println("appendHistory user:", err)
	}
	if err := appendHistory(ctx, UserID, "assistant", answer); err != nil {
		log.Println("appendHistory assistant:", err)
	}

	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   answer,
	})
	if err != nil {
		log.Println("handleText - send response", err)
		return
	}

}

func handleImage(ctx context.Context, b *bot.Bot, update *models.Update, UserID int64, count int, question string) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Генерирую изображение...",
	})
	if err != nil {
		log.Println(err)
		return
	}

	params := openai.ImageGenerateParams{
		//Instructions: openai.String("Отвечай максимум 10 словами."),
		Prompt:  question,
		Model:   openai.ImageModelGPTImage1Mini,
		N:       openai.Int(1),
		Size:    openai.ImageGenerateParamsSize1024x1024,
		Quality: openai.ImageGenerateParamsQualityLow,
	}

	response, err := aiClient.Images.Generate(ctx, params)
	if err != nil {
		log.Println(err)
		return
	}
	answer, err := base64.StdEncoding.DecodeString(response.Data[0].B64JSON)
	if err != nil {
		log.Println(err)
		return
	}

	// Для ImageModelGPTImage1
	_, err = b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID: update.Message.Chat.ID,
		Photo: &models.InputFileUpload{
			Filename: "image.png",
			Data:     bytes.NewReader(answer),
		},
	})

	if err != nil {
		log.Println(err)
		return
	}
	incrementRequestCount(ctx, UserID)
}

func modelChoicesHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{
					Text: "gemma3:1b (Самая быстрая)", CallbackData: "model:gemma3:1b",
				},
			},
			{
				{
					Text: "rnj-1 (Для математических задач)", CallbackData: "model:rnj-1",
				},
			},
		},
	}
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      update.Message.Chat.ID,
		Text:        "Выбери модель:",
		ReplyMarkup: kb,
	})
	if err != nil {
		log.Println(err)
		return
	}
}

func modelsSelectHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserID := update.CallbackQuery.From.ID
	modelName := strings.TrimPrefix(update.CallbackQuery.Data, "model:")

	ensureUser(ctx, UserID)
	setUserModel(ctx, UserID, modelName)

	_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            "Модель выбрана: " + modelName,
	})
	if err != nil {
		log.Println(err)
		return
	}

	_, err = b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    update.CallbackQuery.Message.Message.Chat.ID,
		MessageID: update.CallbackQuery.Message.Message.ID,
		Text:      "Выбрана модель: " + modelName,
	})
	if err != nil {
		log.Println(err)
		return
	}
}
