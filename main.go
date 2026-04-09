package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var SeenUsers sync.Map
var UserStates sync.Map
var UserLastResponseID sync.Map
var UserRequestCount sync.Map
var UserMode sync.Map
var UserTextHistory sync.Map
var aiClient openai.Client
var ollamaClient openai.Client

const (
	StateIdle    = "idle"
	StateWaiting = "waiting"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := godotenv.Load(); err != nil {
		log.Fatal("ENV")
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
		option.WithBaseURL("http://localhost:11434/v1/"),
	)

	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeExact, startHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/clear", bot.MatchTypeExact, clearHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/clear_requests", bot.MatchTypeExact, clearRequestsHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/text", bot.MatchTypeExact, textModeHandler)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/image", bot.MatchTypeExact, imageModeHandler)

	user, _ := b.GetMe(ctx)
	fmt.Printf("Bot name: %#v\n", user.Username)

	_, err = b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Запуск бота"},
			{Command: "text", Description: "Текстовый режим"},
			{Command: "image", Description: "Режим генерации изображений"},
			{Command: "clear", Description: "Очистить историю"},
		},
	})
	if err != nil {
		log.Println("SetMyCommands", err)
	}

	b.Start(ctx)
}

func startHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserId := update.Message.From.ID
	UserName := update.Message.From.Username
	UserFirstName := update.Message.From.FirstName
	Premium := update.Message.From.IsPremium

	UserStates.Store(UserId, StateWaiting)
	UserMode.Store(UserId, "text")

	if _, loaded := SeenUsers.LoadOrStore(UserId, true); !loaded {
		fmt.Printf("New user:\n id: %d\n Name: @%s\n Premium: %t\n", UserId, UserName, Premium)
		fmt.Printf("Начало пользования в %s\n", time.Now())

	}

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
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
	userID := update.Message.From.ID
	state, _ := UserStates.LoadOrStore(userID, StateIdle)

	if state != StateWaiting {
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

	count := 0
	if v, ok := UserRequestCount.Load(userID); ok {
		count = v.(int)
	}
	if count >= 5 {
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

	fmt.Println(userID, " - ", count)

	mode := "text"
	if m, ok := UserMode.Load(userID); ok {
		mode = m.(string)
	}

	question := update.Message.Text

	switch mode {
	case "text":
		handleText(ctx, b, update, userID, question)
	case "image":
		handleImage(ctx, b, update, userID, count, question)
	}
}

func clearHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := update.Message.From.ID
	UserTextHistory.Delete(userID)
	//UserLastResponseID.Delete(update.Message.From.ID)

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
	UserRequestCount.Delete(update.Message.From.ID)

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
	UserMode.Store(update.Message.From.ID, "text")
	UserStates.Store(update.Message.From.ID, StateWaiting)

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
			return
		}
		return
	}
}

func imageModeHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	UserMode.Store(update.Message.From.ID, "image")
	UserStates.Store(update.Message.From.ID, StateWaiting)

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
			return
		}
		return

	}
}

func handleText(ctx context.Context, b *bot.Bot, update *models.Update, userID int64, question string) {
	var history []openai.ChatCompletionMessageParamUnion
	if h, ok := UserTextHistory.Load(userID); ok {
		history = h.([]openai.ChatCompletionMessageParamUnion)
	}

	history = append(history, openai.UserMessage(question))

	response, err := ollamaClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel("qwen3:8b"),
		Messages: append([]openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(""),
			openai.UserMessage(question),
		}, history...),
	})
	if err != nil {
		log.Println("handleText - send question", err)
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "❌ Ошибка, попробуйте еще разочек",
		})
		if err != nil {
			return
		}
		return
	}

	answer := response.Choices[0].Message.Content
	history = append(history, openai.AssistantMessage(answer))
	UserTextHistory.Store(userID, history)

	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   answer,
	})
	if err != nil {
		log.Println("handleText - send response", err)
		return
	}

}

func handleImage(ctx context.Context, b *bot.Bot, update *models.Update, userID int64, count int, question string) {
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
		UserRequestCount.Store(userID, count)
		return
	}
	answer, err := base64.StdEncoding.DecodeString(response.Data[0].B64JSON)

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
	UserRequestCount.Store(userID, count+1)
}
