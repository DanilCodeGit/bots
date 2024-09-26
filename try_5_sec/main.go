package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/telebot.v3"
)

const (
	intervalDuration     = 5 * time.Second // тайминг свечи
	minVolume            = 100.0           // минимальный объем
	priceChangeThreshold = 1.0             // процент отклонения

	webSocketURL = "wss://api.gateio.ws/ws/v4/"
	botToken     = "7385636289:AAF54ZXLNkImvDB_x2IgvTSbXJx0OC2ymSU"
	apiURL       = "https://api.gateio.ws/api/v4/spot/currency_pairs"
)

var (
	registeredChatID int64
	candlestickData  = make(map[string][]Trade)
	mutex            sync.Mutex        // Mutex to synchronize access to candlestickData
	stopChan         = make(chan bool) // Channel to signal stopping
)

type Trade struct {
	ID           int    `json:"id"`
	CreateTime   int64  `json:"create_time"`
	CreateTimeMs string `json:"create_time_ms"`
	Side         string `json:"side"`
	CurrencyPair string `json:"currency_pair"`
	Amount       string `json:"amount"`
	Price        string `json:"price"`
	Range        string `json:"range"`
}

type WebSocketMessage struct {
	Time      int64           `json:"time"`
	TimeMs    int64           `json:"time_ms"`
	Channel   string          `json:"channel"`
	Event     string          `json:"event"`
	Result    json.RawMessage `json:"result"`
	Error     *ErrorMessage   `json:"error,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
}

type ErrorMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type CurrencyPair struct {
	ID string `json:"id"`
}

func main() {
	pref := telebot.Settings{
		Token:  botToken,
		Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
	}

	bot, err := telebot.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}

	bot.Handle("/start", func(c telebot.Context) error {
		registeredChatID = c.Chat().ID
		go startWebSocketAndSendNotifications(bot) // Запускаем горутину при старте
		return c.Send("Вы успешно зарегистрированы для получения уведомлений!")
	})

	bot.Handle("/stop", func(c telebot.Context) error {
		stopChan <- true // Send stop signal
		return c.Send("Уведомления остановлены.")
	})

	bot.Start()
}

func startWebSocketAndSendNotifications(bot *telebot.Bot) {
	pairs, err := getUSDTTradingPairs()
	if err != nil {
		log.Fatal("Ошибка получения списка торговых пар:", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(webSocketURL, nil)
	if err != nil {
		log.Fatal("Ошибка подключения к WebSocket:", err)
	}
	defer conn.Close()

	for _, pair := range pairs {
		subscribeMessage := map[string]interface{}{
			"time":    int(time.Now().Unix()),
			"channel": "spot.trades",
			"event":   "subscribe",
			"payload": []string{pair},
		}

		err = conn.WriteJSON(subscribeMessage)
		if err != nil {
			log.Fatal("Ошибка отправки подписки:", err)
		}
	}

	ticker := time.NewTicker(intervalDuration)

	go func() {
		for {
			select {
			case <-stopChan:
				log.Println("Остановлено получение уведомлений.")
				return
			default:
				_, message, err := conn.ReadMessage()
				if err != nil {
					log.Println("Ошибка получения сообщения:", err)
					continue
				}

				var wsMessage WebSocketMessage
				err = json.Unmarshal(message, &wsMessage)
				if err != nil {
					log.Println("Ошибка парсинга сообщения:", err)
					log.Printf("Сообщение: %s", message)
					continue
				}

				switch wsMessage.Event {
				case "subscribe":
					// Обработка подтверждения подписки
					log.Printf("Подписка на %s подтверждена.", wsMessage.Channel)
					continue
				case "update":
					// Обработка обновлений сделок
					var trade Trade
					err = json.Unmarshal(wsMessage.Result, &trade)
					if err != nil {
						log.Println("Ошибка парсинга сделки:", err)
						log.Printf("Сообщение: %s", message)
						continue
					}

					mutex.Lock()
					candlestickData[trade.CurrencyPair] = append(candlestickData[trade.CurrencyPair], trade)
					mutex.Unlock()
				default:
					// Обработка других событий при необходимости
					continue
				}

				if wsMessage.Error != nil {
					log.Printf("Ошибка от сервера: %s", wsMessage.Error.Message)
				}
			}
		}
	}()

	for {
		select {
		case <-stopChan:
			log.Println("Остановлено формирование свечей.")
			return
		case <-ticker.C:
			mutex.Lock()
			for pair, trades := range candlestickData {
				if len(trades) == 0 {
					continue
				}

				openPrice, err1 := strconv.ParseFloat(trades[0].Price, 64)
				closePrice, err2 := strconv.ParseFloat(trades[len(trades)-1].Price, 64)
				if err1 != nil || err2 != nil {
					log.Println("Ошибка преобразования цены открытия или закрытия:", err1, err2)
					continue
				}

				highPrice := openPrice
				lowPrice := openPrice
				totalVolume := 0.0

				for _, trade := range trades {
					if trade.Price == "" || trade.Amount == "" {
						log.Println("Пропущена сделка с пустой ценой или количеством")
						continue
					}

					price, err1 := strconv.ParseFloat(trade.Price, 64)
					amount, err2 := strconv.ParseFloat(trade.Amount, 64)
					if err1 != nil || err2 != nil {
						log.Println("Ошибка преобразования строки в число:", err1, err2)
						continue
					}

					// Денежный объём сделки
					tradeVolume := amount * price
					totalVolume += tradeVolume

					if price > highPrice {
						highPrice = price
					}
					if price < lowPrice {
						lowPrice = price
					}
				}

				priceChangePercent := ((closePrice - openPrice) / openPrice) * 100

				if totalVolume > minVolume && math.Abs(priceChangePercent) > priceChangeThreshold {
					if registeredChatID != 0 {
						link := createTradingViewLink(pair)
						formattedVolume := formatVolume(totalVolume)
						formattedPriceChange := getPriceChangeWithSign(openPrice, closePrice)

						message := fmt.Sprintf(
							"%s\n%s\n💰 Vol: %s$\n📈 %s",
							formatPair(pair), formattedPriceChange, formattedVolume, link,
						)

						_, err := bot.Send(telebot.ChatID(registeredChatID), message)
						if err != nil {
							log.Printf("Ошибка отправки сообщения в Telegram: %v", err)
						}
					}
				}

				candlestickData[pair] = nil
			}
			mutex.Unlock()
		}
	}
}

func createTradingViewLink(symbol string) string {
	symbol = strings.ReplaceAll(symbol, "_", "")
	baseURL := "https://ru.tradingview.com/symbols/"
	link := fmt.Sprintf("%s%s/?exchange=GATEIO", baseURL, symbol)
	link = strings.ReplaceAll(link, "1m", "")
	link = strings.ReplaceAll(link, "5s", "")
	return link
}

func getUSDTTradingPairs() ([]string, error) {
	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения торговых пар: %v", err)
	}
	defer resp.Body.Close()

	var currencyPairs []CurrencyPair
	err = json.NewDecoder(resp.Body).Decode(&currencyPairs)
	if err != nil {
		return nil, fmt.Errorf("ошибка декодирования ответа: %v", err)
	}

	var usdtPairs []string
	for _, pair := range currencyPairs {
		if strings.HasSuffix(pair.ID, "_USDT") {
			usdtPairs = append(usdtPairs, pair.ID)
		}
	}

	return usdtPairs, nil
}

func formatPair(text string) string {
	text = strings.Replace(text, "_", "/", 1)
	return text
}

func formatVolume(volume float64) string {
	if volume > 1e9 {
		return fmt.Sprintf("%.3e", volume)
	}
	return fmt.Sprintf("%.2f", volume)
}

func getPriceChangeWithSign(openPrice, closePrice float64) string {
	priceChangePercent := ((closePrice - openPrice) / openPrice) * 100
	if priceChangePercent > 0 {
		return fmt.Sprintf("🟢 +%.2f%%", priceChangePercent)
	}
	return fmt.Sprintf("🔴 %.2f%%", priceChangePercent)
}
