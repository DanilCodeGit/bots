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
	webSocketURL = "wss://api.gateio.ws/ws/v4/"
	finaltoken   = "7385636289:AAF54ZXLNkImvDB_x2IgvTSbXJx0OC2ymSU"
	apiURL       = "https://api.gateio.ws/api/v4/spot/currency_pairs"
)

var (
	candlestickData = make(map[string][]Trade)
	mutex           sync.RWMutex
	stopChan        chan bool
	isRunning       bool
	runMutex        sync.Mutex

	userSettingsMutex sync.Mutex
	userSettings      = make(map[int64]*UserSettings)
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

type UserSettings struct {
	MinVolume            float64
	PriceChangeThreshold float64
	IntervalDuration     time.Duration
	LastProcessedTime    time.Time
}

type PairMetrics struct {
	OpenPrice          float64
	ClosePrice         float64
	HighPrice          float64
	LowPrice           float64
	TotalVolume        float64
	PriceChangePercent float64
}

func main() {
	pref := telebot.Settings{
		Token:  finaltoken,
		Poller: &telebot.LongPoller{Timeout: 5 * time.Second},
	}

	bot, err := telebot.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}

	bot.Handle("/start", func(c telebot.Context) error {
		chatID := c.Chat().ID
		userSettingsMutex.Lock()
		if _, exists := userSettings[chatID]; exists {
			userSettingsMutex.Unlock()
			return c.Send("Вы уже зарегистрированы для получения уведомлений!")
		}
		userSettings[chatID] = &UserSettings{
			MinVolume:            1000.0,
			PriceChangeThreshold: 2.0,
			IntervalDuration:     5 * time.Second,
			LastProcessedTime:    time.Now(),
		}
		userSettingsMutex.Unlock()

		runMutex.Lock()
		if !isRunning {
			isRunning = true
			runMutex.Unlock()
			stopChan = make(chan bool)
			go startWebSocketAndSendNotifications(bot) // Запускаем горутину при старте
		} else {
			runMutex.Unlock()
		}

		return c.Send("Вы успешно зарегистрированы для получения уведомлений!")
	})

	bot.Handle("/percent", func(c telebot.Context) error {
		log.Println("Received command: /percent")
		args := c.Args()
		if len(args) == 0 {
			return c.Send("Пожалуйста, укажите процент отклонения, например: /percent 2.5")
		}
		value, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			return c.Send("Неверный формат числа. Пожалуйста, введите число, например: /percent 2.5")
		}

		chatID := c.Chat().ID
		userSettingsMutex.Lock()
		if settings, exists := userSettings[chatID]; exists {
			settings.PriceChangeThreshold = value
			userSettingsMutex.Unlock()
			return c.Send(fmt.Sprintf("Процент отклонения успешно установлен на %.2f%%", value))
		} else {
			userSettingsMutex.Unlock()
			return c.Send("Вы не зарегистрированы. Пожалуйста, введите /start для регистрации.")
		}
	})

	bot.Handle("/volume", func(c telebot.Context) error {
		log.Println("Received command: /volume")
		args := c.Args()
		if len(args) == 0 {
			return c.Send("Пожалуйста, укажите минимальный объем, например: /volume 5000")
		}
		value, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			return c.Send("Неверный формат числа. Пожалуйста, введите число, например: /volume 5000")
		}

		chatID := c.Chat().ID
		userSettingsMutex.Lock()
		if settings, exists := userSettings[chatID]; exists {
			settings.MinVolume = value
			userSettingsMutex.Unlock()
			return c.Send(fmt.Sprintf("Минимальный объем успешно установлен на %.2f", value))
		} else {
			userSettingsMutex.Unlock()
			return c.Send("Вы не зарегистрированы. Пожалуйста, введите /start для регистрации.")
		}
	})

	bot.Handle("/interval", func(c telebot.Context) error {
		log.Println("Received command: /interval")
		args := c.Args()
		if len(args) == 0 {
			return c.Send("Пожалуйста, укажите интервал свечи, например: /interval 10s или 1m")
		}
		value, err := time.ParseDuration(args[0])
		if err != nil {
			return c.Send("Неверный формат интервала. Используйте форматы как 10s, 1m, 2h")
		}

		chatID := c.Chat().ID
		userSettingsMutex.Lock()
		if settings, exists := userSettings[chatID]; exists {
			settings.IntervalDuration = value
			userSettingsMutex.Unlock()
			return c.Send(fmt.Sprintf("Интервал свечи успешно установлен на %s", value))
		} else {
			userSettingsMutex.Unlock()
			return c.Send("Вы не зарегистрированы. Пожалуйста, введите /start для регистрации.")
		}
	})

	bot.Start()
}

func startWebSocketAndSendNotifications(bot *telebot.Bot) {
	defer func() {
		runMutex.Lock()
		isRunning = false
		runMutex.Unlock()
	}()

	log.Println("Starting WebSocket connection")

	pairs, err := getUSDTTradingPairs()
	if err != nil {
		log.Fatal("Ошибка получения списка торговых пар:", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(webSocketURL, nil)
	if err != nil {
		log.Println("Ошибка подключения к WebSocket:", err)
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
			log.Println("Ошибка отправки подписки:", err)
		}
	}

	go readWebSocketMessages(conn)

	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			log.Println("Остановлено формирование свечей.")
			return
		case <-ticker.C:
			processCandlestickData(bot)
		}
	}
}

func readWebSocketMessages(conn *websocket.Conn) {
	log.Println("Started reading WebSocket messages")
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Ошибка получения сообщения:", err)
			return
		}

		var wsMessage WebSocketMessage
		err = json.Unmarshal(message, &wsMessage)
		if err != nil {
			log.Println("Ошибка парсинга сообщения:", err)
			continue
		}

		if wsMessage.Event == "update" {
			var trade Trade
			err = json.Unmarshal(wsMessage.Result, &trade)
			if err != nil {
				log.Println("Ошибка парсинга сделки:", err)
				continue
			}

			mutex.Lock()
			candlestickData[trade.CurrencyPair] = append(candlestickData[trade.CurrencyPair], trade)
			mutex.Unlock()
		}
	}
}

func processCandlestickData(bot *telebot.Bot) {
	log.Println("Processing candlestick data")
	mutex.RLock()
	if len(candlestickData) == 0 {
		mutex.RUnlock()
		return
	}

	candlestickDataCopy := make(map[string][]Trade)
	for pair, trades := range candlestickData {
		tradesCopy := make([]Trade, len(trades))
		copy(tradesCopy, trades)
		candlestickDataCopy[pair] = tradesCopy
	}
	mutex.RUnlock()

	var wg sync.WaitGroup
	for pair, trades := range candlestickDataCopy {
		wg.Add(1)
		go func(pair string, trades []Trade) {
			defer wg.Done()
			metrics := computeMetrics(trades)
			if metrics != nil {
				log.Printf("Metrics computed for pair %s: %+v", pair, metrics)
				notifyUsers(pair, metrics, bot)
			}
		}(pair, trades)
	}
	wg.Wait()

	mutex.Lock()
	for pair := range candlestickData {
		candlestickData[pair] = nil
	}
	mutex.Unlock()
}

func notifyUsers(pair string, metrics *PairMetrics, bot *telebot.Bot) {
	log.Printf("Sending notifications for pair %s", pair)
	userSettingsMutex.Lock()
	defer userSettingsMutex.Unlock()

	currentTime := time.Now()
	for chatID, settings := range userSettings {
		if currentTime.Sub(settings.LastProcessedTime) >= settings.IntervalDuration &&
			metrics.TotalVolume > settings.MinVolume &&
			math.Abs(metrics.PriceChangePercent) > settings.PriceChangeThreshold {

			settings.LastProcessedTime = currentTime

			formattedVolume := formatVolume(metrics.TotalVolume)
			formattedPriceChange := getPriceChangeWithSign(metrics.OpenPrice, metrics.ClosePrice)

			message := fmt.Sprintf(
				"%s\n%s\n💰 Vol: %s$\n",
				formatPair(pair), formattedPriceChange, formattedVolume,
			)
			_, err := bot.Send(telebot.ChatID(chatID), message)
			if err != nil {
				log.Printf("Ошибка отправки сообщения в Telegram: %v", err)
			} else {
				log.Printf("Сообщение отправлено пользователю %d для пары %s", chatID, pair)
			}
		}
	}
}

func computeMetrics(trades []Trade) *PairMetrics {
	if len(trades) == 0 {
		return nil
	}

	openPrice, err1 := strconv.ParseFloat(trades[0].Price, 64)
	closePrice, err2 := strconv.ParseFloat(trades[len(trades)-1].Price, 64)
	if err1 != nil || err2 != nil {
		log.Println("Ошибка преобразования цены открытия или закрытия:", err1, err2)
		return nil
	}

	highPrice := openPrice
	lowPrice := openPrice
	totalVolume := 0.0

	for _, trade := range trades {
		price, err1 := strconv.ParseFloat(trade.Price, 64)
		amount, err2 := strconv.ParseFloat(trade.Amount, 64)
		if err1 != nil || err2 != nil {
			log.Println("Ошибка преобразования строки в число:", err1, err2)
			continue
		}

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

	return &PairMetrics{
		OpenPrice:          openPrice,
		ClosePrice:         closePrice,
		HighPrice:          highPrice,
		LowPrice:           lowPrice,
		TotalVolume:        totalVolume,
		PriceChangePercent: priceChangePercent,
	}
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
	return strings.Replace(text, "_", "/", 1)
}

func formatVolume(volume float64) string {
	if volume > 1e9 {
		return fmt.Sprintf("%.3e", volume)
	}
	return fmt.Sprintf("%.2f", volume)
}

func getPriceChangeWithSign(openPrice, closePrice float64) string {
	priceChangePercent := ((closePrice - openPrice) / openPrice) * 100
	circles := getCirclesForPercent(priceChangePercent)
	if priceChangePercent > 0 {
		return fmt.Sprintf("%s +%.2f%%", circles, priceChangePercent)
	}
	return fmt.Sprintf("%s %.2f%%", circles, priceChangePercent)
}

func getCirclesForPercent(priceChangePercent float64) string {
	absPercent := math.Abs(priceChangePercent)
	var circles string

	if absPercent < 10 {
		circles = "🟢"
	} else if absPercent > 10 && absPercent < 20 {
		circles = "🟢🟢"
	} else if absPercent > 20 && absPercent < 50 {
		circles = "🟢🟢🟢"
	} else {
		circles = "🟢🟢🟢🟢"
	}

	if priceChangePercent < 0 {
		circles = strings.ReplaceAll(circles, "🟢", "🔴")
	}

	return circles
}
