package main

import (
	"database/sql"
	"io/ioutil"
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v2"
)

type Config struct {
	TelegramBotToken string `yaml:"telegram_bot_token"`
}

func main() {

	config := Config{}
	configFile, err := ioutil.ReadFile("secrets.yml")
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

	db, err := sql.Open("sqlite3", "./messages.db")
	if err != nil {
		log.Panic(err)
	}
	defer db.Close()

	createTable(db)

	updates, err := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

		insertMessage(db, update.Message.From.UserName, update.Message.Text)

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Message received")
		bot.Send(msg)
	}
}

func createTable(db *sql.DB) {
	sqlStmt := `
    CREATE TABLE IF NOT EXISTS messages (
        id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
        username TEXT,
        message TEXT
    );
    `
	_, err := db.Exec(sqlStmt)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlStmt)
		return
	}
}

func insertMessage(db *sql.DB, username string, message string) {
	tx, err := db.Begin()
	if err != nil {
		log.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO messages(username, message) VALUES(?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()
	_, err = stmt.Exec(username, message)
	if err != nil {
		log.Fatal(err)
	}
	tx.Commit()
}
