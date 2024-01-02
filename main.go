package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"gopkg.in/yaml.v2"
)

type Config struct {
	TelegramBotToken string `yaml:"telegram_bot_token"`
}

var awaitingInput = make(map[int64]string)
var userSettings = make(map[int64]UserSettings)
var messageBuffers = make(map[int64]*bytes.Buffer)

type UserSettings struct {
	Token  string
	Domain string
}

func main() {
	config := Config{}
	configFile, err := os.ReadFile("secrets.yml")
	if err != nil {
		log.Panic(err)
	}
	err = yaml.Unmarshal(configFile, &config)
	if err != nil {
		log.Panic(err)
	}

	bot, err := tgbotapi.NewBotAPI(config.TelegramBotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Panic(err)
	}

	loadUserSettingsFromFile("tokens.txt", "token")
	loadUserSettingsFromFile("domains.txt", "domain")

	checkTicker := time.NewTicker(5 * time.Minute)
	go func() {
		for range checkTicker.C {
			checkAndSendOldestPosts(bot)
		}
	}()

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID

		if update.Message.IsCommand() {
			if update.Message.Command() == "send" {
				handleSendCommand(chatID, bot)
				continue
			}

			handleCommand(update.Message, bot)
		} else if inputType, ok := awaitingInput[chatID]; ok {
			handleUserInput(chatID, inputType, update.Message.Text)
		} else {
			bufferMessage(chatID, update.Message.Text)
		}
	}
}

func checkAndSendOldestPosts(bot *tgbotapi.BotAPI) {
	for userID, settings := range userSettings {
		if shouldSendPost(settings) {
			sendOldestPost(bot, userID, settings)
		}
	}
}

func shouldSendPost(settings UserSettings) bool {
	lastPostTime, err := getLastPostTime(settings.Domain, settings.Token)
	if err != nil {
		log.Printf("Error fetching last post time: %v", err)
		return false
	}

	fmt.Printf("Last post time: %v\n", lastPostTime)

	return time.Since(lastPostTime) >= 4*time.Hour
}

func getLastPostTime(domain, token string) (time.Time, error) {
	userID, err := getUserID(domain, token) // You'll need to implement this function
	if err != nil {
		return time.Time{}, fmt.Errorf("error getting user ID: %w", err)
	}

	apiUrl := fmt.Sprintf("https://%s/api/v1/accounts/%d/statuses", domain, userID)

	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("error fetching user statuses: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return time.Time{}, fmt.Errorf("received non-success status code %d", resp.StatusCode)
	}

	var posts []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil {
		return time.Time{}, fmt.Errorf("error decoding response: %w", err)
	}

	for _, post := range posts {
		if inReplyToID, exists := post["in_reply_to_id"]; !exists || inReplyToID == nil {
			createdAt, ok := post["created_at"].(string)
			if !ok {
				continue
			}
			postTime, err := time.Parse(time.RFC3339, createdAt)
			if err != nil {
				log.Printf("Error parsing time: %v", err)
				continue
			}
			return postTime, nil
		}
	}

	return time.Time{}, errors.New("no valid posts found")
}

func getUserID(domain, token string) (int64, error) {
	apiUrl := fmt.Sprintf("https://%s/api/v1/accounts/verify_credentials", domain)

	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return 0, fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("error fetching user credentials: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("received non-success status code %d", resp.StatusCode)
	}

	var userData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&userData); err != nil {
		return 0, fmt.Errorf("error decoding response: %w", err)
	}

	id, ok := userData["id"].(string)
	if !ok {
		return 0, errors.New("user ID not found in response")
	}

	userID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing user ID: %w", err)
	}

	return userID, nil
}

func sendOldestPost(bot *tgbotapi.BotAPI, userID int64, settings UserSettings) {
	dirPath := fmt.Sprintf("posts/%d", userID)
	files, err := ioutil.ReadDir(dirPath)
	if err != nil {
		log.Printf("Error reading directory for user %d: %v", userID, err)
		return
	}
	if len(files) == 0 {
		return // No posts to send for this user
	}

	// Assuming files are named with Unix timestamps, the oldest will be the first
	oldestFile := files[0]
	for _, file := range files {
		if file.ModTime().Before(oldestFile.ModTime()) {
			oldestFile = file
		}
	}

	filePath := filepath.Join(dirPath, oldestFile.Name())
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Printf("Error reading file for user %d: %v", userID, err)
		return
	}

	// Post content to Mastodon
	postLink, err := postToMastodon(settings.Domain, settings.Token, string(content))
	if err != nil {
		log.Printf("Error posting to Mastodon for user %d: %v", userID, err)
		return
	}

	// Delete the file after successful post
	if err := os.Remove(filePath); err != nil {
		log.Printf("Error deleting file for user %d: %v", userID, err)
		return
	}

	// Notify the user about the successful post
	message := fmt.Sprintf("Post sent: %s", postLink)
	msg := tgbotapi.NewMessage(userID, message)
	bot.Send(msg)
}

func handleSendCommand(chatID int64, bot *tgbotapi.BotAPI) {
	if buffer, ok := messageBuffers[chatID]; ok && buffer.Len() > 0 {
		err := savePostToFile(chatID, buffer.String())
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Error saving post: %v", err)))
		} else {
			bot.Send(tgbotapi.NewMessage(chatID, "Messages saved to file."))
		}
		buffer.Reset()
	} else {
		bot.Send(tgbotapi.NewMessage(chatID, "No messages to save."))
	}
}

func savePostToFile(userID int64, content string) error {
	dirPath := fmt.Sprintf("posts/%d", userID)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return fmt.Errorf("error creating directory: %w", err)
	}

	filePath := fmt.Sprintf("%s/%d.txt", dirPath, time.Now().Unix())
	return ioutil.WriteFile(filePath, []byte(content), 0644)
}

func bufferMessage(chatID int64, message string) {
	if _, ok := messageBuffers[chatID]; !ok {
		messageBuffers[chatID] = new(bytes.Buffer)
	}
	buffer := messageBuffers[chatID]
	if buffer.Len() > 0 {
		buffer.WriteString("\n\n")
	}
	buffer.WriteString(message)
}

func handleCommand(message *tgbotapi.Message, bot *tgbotapi.BotAPI) {
	chatID := message.Chat.ID
	switch message.Command() {
	case "token":
		awaitingInput[chatID] = "token"
		bot.Send(tgbotapi.NewMessage(chatID, "Please send your Mastodon access token."))
	case "domain":
		awaitingInput[chatID] = "domain"
		bot.Send(tgbotapi.NewMessage(chatID, "Please send your Mastodon instance domain."))
	case "help":
		commands := "Commands:\n/token - Set your Mastodon access token\n/domain - Set your Mastodon instance domain\n/help - Show this help message"
		bot.Send(tgbotapi.NewMessage(chatID, commands))
	default:
		bot.Send(tgbotapi.NewMessage(chatID, "I don't know that command"))
	}
}

func handleUserInput(chatID int64, inputType, input string) {
	sanitizedInput := sanitizeInput(input)
	settings, _ := userSettings[chatID]
	switch inputType {
	case "token":
		settings.Token = sanitizedInput
		saveToFile("tokens.txt", chatID, sanitizedInput)
	case "domain":
		settings.Domain = sanitizedInput
		saveToFile("domains.txt", chatID, sanitizedInput)
	}
	userSettings[chatID] = settings
}

func defaultResponse(chatID int64, bot *tgbotapi.BotAPI) {
	bot.Send(tgbotapi.NewMessage(chatID, "Welcome! Please use a command to get started. Type /help for a list of commands."))
}

func postToMastodon(domain, token, content string) (string, error) {
	apiUrl := fmt.Sprintf("https://%s/api/v1/statuses", domain)
	data := url.Values{"status": {content}}

	req, err := http.NewRequest("POST", apiUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error posting to Mastodon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("received non-success status code %d", resp.StatusCode)
	}

	// Parse the response to extract the link to the post
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("error decoding response: %w", err)
	}

	// Assuming 'url' field in response contains the post link
	postLink, ok := result["url"].(string)
	if !ok {
		return "", errors.New("post link not found in response")
	}

	return postLink, nil
}

func saveToFile(filename string, userID int64, data string) {
	filePath := filename
	content, err := os.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	lines := strings.Split(string(content), "\n")
	newLine := fmt.Sprintf("%d: %s", userID, data)
	updated := false

	for i, line := range lines {
		if strings.HasPrefix(line, fmt.Sprintf("%d:", userID)) {
			lines[i] = newLine
			updated = true
			break
		}
	}

	if !updated {
		lines = append(lines, newLine)
	}

	err = ioutil.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0644)
	if err != nil {
		log.Fatal(err)
	}
}

func sanitizeInput(input string) string {
	return strings.TrimSpace(strings.Split(input, "\n")[0])
}

func loadUserSettingsFromFile(filename, settingType string) {
	file, err := os.Open(filename)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Fatal(err)
		}
		return // File does not exist, so nothing to load
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ": ")
		if len(parts) != 2 {
			continue // Invalid line format
		}

		userID, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			log.Printf("Error parsing user ID from file: %v", err)
			continue
		}

		setting := strings.TrimSpace(parts[1])
		if settingType == "token" {
			userSettings[userID] = UserSettings{
				Token:  setting,
				Domain: userSettings[userID].Domain,
			}
		} else if settingType == "domain" {
			userSettings[userID] = UserSettings{
				Token:  userSettings[userID].Token,
				Domain: setting,
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}
