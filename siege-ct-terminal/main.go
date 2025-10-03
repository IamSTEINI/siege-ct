package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ORDER struct {
	Amount    string `json:"amount"`
	ID        string `json:"id"`
	OrderType string `json:"order_type"`
	Price     string `json:"price"`
	Score     string `json:"score"`
	Ticker    string `json:"ticker"`
	Token     string `json:"token"`
}

type OB struct {
	Orderbook ORDERBOOK `json:"orderbook"`
}

type ORDERBOOK struct {
	Buy  []ORDER `json:"buy"`
	Sell []ORDER `json:"sell"`
}

var token string = ""
var delay time.Duration = 1 * time.Second // DELAY or server gets cooked
var trading_ws *websocket.Conn
var ws_mutex sync.Mutex

type TradeResponse struct {
	Status  string      `json:"status"`
	Message string      `json:"message"`
	Order   interface{} `json:"order,omitempty"`
}

type AssetResponse struct {
	Status  string                   `json:"status"`
	Message string                   `json:"message"`
	Assets  []map[string]interface{} `json:"assets,omitempty"`
}

type CreateUserResponse struct {
	Status  string                 `json:"status"`
	Message string                 `json:"message"`
	Account map[string]interface{} `json:"account,omitempty"`
}

func clearScreen() {
	switch runtime.GOOS {
	case "linux", "darwin":
		cmd := exec.Command("clear")
		cmd.Stdout = os.Stdout
		cmd.Run()
	case "windows":
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Run()
	}
}

type OrderLevel struct {
	Price  float64
	Amount float64
}

// HOW DO WE DETERMINE THE MARKET PRICE?
// -> VWAP Volume weighted average price

func vwap(orders map[float64]float64, limit int, descending bool) float64 {
	prices := make([]float64, 0, len(orders))
	for p := range orders {
		prices = append(prices, p)
	}

	sort.Slice(prices, func(i, j int) bool {
		if descending {
			return prices[i] > prices[j]
		}
		return prices[i] < prices[j]
	})

	var total_volume float64
	var total_price_volume float64
	count := 0

	for _, price := range prices {
		volume := orders[price]
		total_volume += volume
		total_price_volume += price * volume

		count += 1

		if limit > 0 && count >= limit {
			break
		}
	}

	if total_volume == 0 {
		return 0
	}

	return total_price_volume / total_volume
}

func calculate_market_price(buys map[float64]float64, sells map[float64]float64) float64 {
	// Getting the top 10!!
	buys_vwap := vwap(buys, 10, true)
	sells_vwap := vwap(sells, 10, false)

	if buys_vwap > 0 && sells_vwap > 0 {
		return (buys_vwap + sells_vwap) / 2
	}

	if buys_vwap > 0 {
		return buys_vwap
	}

	return sells_vwap
}

func orderbook_display(orderbook OB, ticker string) {
	fmt.Print("\033[2J\033[H")

	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, _ := cmd.Output()
	width := 80
	if len(out) > 0 {
		parts := strings.Split(strings.TrimSpace(string(out)), " ")
		if len(parts) >= 2 {
			if w, err := strconv.Atoi(parts[1]); err == nil {
				width = w
			}
		}
	}

	modified_buy_orders := make(map[float64]float64)
	modified_sell_orders := make(map[float64]float64)

	for _, order := range orderbook.Orderbook.Buy {
		price, err := strconv.ParseFloat(order.Price, 64)
		amount, amtErr := strconv.ParseFloat(order.Amount, 64)
		ticker = order.Ticker
		if err == nil && amtErr == nil {
			modified_buy_orders[price] += amount
		}
	}

	for _, order := range orderbook.Orderbook.Sell {
		price, err := strconv.ParseFloat(order.Price, 64)
		amount, amtErr := strconv.ParseFloat(order.Amount, 64)
		ticker = order.Ticker
		if err == nil && amtErr == nil {
			modified_sell_orders[price] += amount
		}
	}

	var market_price float64 = calculate_market_price(modified_buy_orders, modified_sell_orders)

	fmt.Printf("=== ORDERBOOK: %s ===\n\n", ticker)
	fmt.Printf("%-15s   %-15s   %-15s\n", "AMOUNT", "PRICE", "SIDE")
	fmt.Printf("%s\n", strings.Repeat("=", width))

	buyPrices := make([]float64, 0, len(modified_buy_orders))
	for price := range modified_buy_orders {
		buyPrices = append(buyPrices, price)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(buyPrices)))

	maxAmount := 0.0
	for _, price := range buyPrices {
		if modified_buy_orders[price] > maxAmount {
			maxAmount = modified_buy_orders[price]
		}
	}
	for _, price := range modified_sell_orders {
		if price > maxAmount {
			maxAmount = price
		}
	}

	for _, price := range buyPrices {
		amount := modified_buy_orders[price]
		amountStr := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", amount), "0"), ".")
		priceStr := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", price), "0"), ".")

		barWidth := int((amount / maxAmount) * float64(width-50))
		if barWidth < 5 {
			barWidth = 5
		}

		fmt.Printf("\033[42m\033[30m%-15s   %-15s   BUY %s\033[0m\n",
			amountStr, priceStr, strings.Repeat("█", barWidth))
	}

	fmt.Printf("\n%s\n", strings.Repeat("-", width))
	fmt.Printf("\033[33mPrice: %.6f\033[0m\n", market_price)
	fmt.Printf("%s\n\n", strings.Repeat("-", width))

	sellPrices := make([]float64, 0, len(modified_sell_orders))
	for price := range modified_sell_orders {
		sellPrices = append(sellPrices, price)
	}
	sort.Float64s(sellPrices)

	for _, price := range sellPrices {
		amount := modified_sell_orders[price]
		amountStr := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", amount), "0"), ".")
		priceStr := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", price), "0"), ".")

		barWidth := int((amount / maxAmount) * float64(width-50))
		if barWidth < 5 {
			barWidth = 5
		}

		fmt.Printf("\033[41m\033[30m%-15s   %-15s   SELL %s\033[0m\n",
			amountStr, priceStr, strings.Repeat("█", barWidth))
	}

	fmt.Printf("\n\033[36mType 'Q' and press ENTER to quit | Updates every %v\033[0m\n", delay)
}

func enter_to_continue() {
	fmt.Println("Press ENTER to continue...")
	var input string
	fmt.Scanln(&input)
	main_menu()
}

func fetch_tickers() {
	url := "http://localhost:8080/api/tickers"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("Error: could not create request: %s\n", err)
		os.Exit(1)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error while http request: %s\n", err)
		main_menu()
	}
	defer res.Body.Close()
	res_body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Printf("Error while reading body: %s\n", err)
		os.Exit(1)
	}

	type Ticker struct {
		Name   string `json:"ticker"`
		Volume int    `json:"volume"`
	}
	type TickerResponse struct {
		Message string   `json:"message"`
		Status  string   `json:"status"`
		Tickers []Ticker `json:"tickers"`
	}

	var response TickerResponse
	if err := json.Unmarshal(res_body, &response); err != nil {
		fmt.Printf("Error parsing response: %s\n", err)
		os.Exit(1)
	}

	fmt.Println("===- AVAILABLE TICKERS -===")
	for _, ticker := range response.Tickers {
		fmt.Printf("%-15s VOL: %d\n", ticker.Name, ticker.Volume)
	}
	fmt.Println("===========================")

	enter_to_continue()
}

func ticker_ws(ticker string) {
	ticker_ws := url.URL{Scheme: "ws", Host: "localhost:8080", Path: "/ws/ticker/" + strings.Replace(ticker, "/", "-", -1)}
	conn, _, err := websocket.DefaultDialer.Dial(ticker_ws.String(), nil)
	if err != nil {
		log.Fatal("[DIAL ERROR]", err)
	}
	defer conn.Close()

	done := make(chan struct{})
	var doneOnce sync.Once

	go func() {
		defer func() {
			doneOnce.Do(func() {
				close(done)
			})
		}()
		for {
			select {
			case <-done:
				return
			default:
				_, message, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var orderbook OB
				err = json.Unmarshal(message, &orderbook)
				if err != nil {
					return
				}
				orderbook_display(orderbook, ticker)
			}
		}
	}()

	go func() {
		defer func() {
			doneOnce.Do(func() {
				close(done)
			})
		}()
		var input string
		for {
			select {
			case <-done:
				return
			default:
				fmt.Scanln(&input)
				if strings.ToUpper(strings.TrimSpace(input)) == "Q" {
					return
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				err := conn.WriteMessage(websocket.TextMessage, []byte("GET-ORDERBOOK"))
				if err != nil {
					doneOnce.Do(func() {
						close(done)
					})
					return
				}
				time.Sleep(delay)
			}
		}
	}()

	<-done
	clearScreen()
}

func main_menu() {
	clearScreen()
	fmt.Println("=- SIEGE CT TERMINAL -=======- by STEIN -=======[X to EXIT]=")
	if token != "" {
		fmt.Printf("Logged in as: %s\n", token[:8]+"...")
	} else {
		fmt.Println("Not logged in - please create account or login")
	}
	fmt.Println("====================================================")
	fmt.Println("[ 0 ] LIVE TICKER ORDERBOOK")
	fmt.Println("[ 1 ] SEE ALL TICKERS")
	fmt.Println("[ 2 ] CREATE NEW USER")
	fmt.Println("[ 3 ] CHECK MY ASSETS")
	fmt.Println("[ 4 ] PLACE LIMIT ORDER")
	fmt.Println("[ 5 ] EXECUTE MARKET ORDER")
	fmt.Println("[ 6 ] SWITCH USER (LOGIN)")
	fmt.Println("====================================================")
	fmt.Print("SELECT> ")
}

func handle_option(option string) {
	switch option {
	case "0":
		var selected_ticker string
		fmt.Print("TICKER (eg. SIGE/HACK): ")
		fmt.Scanln(&selected_ticker)
		fmt.Println("[~] Connecting to", selected_ticker, "...")
		ticker_ws(selected_ticker)
	case "1":
		fetch_tickers()
	case "2":
		create_new_user()
	case "3":
		check_assets()
	case "4":
		place_limit_order()
	case "5":
		execute_market_order()
	case "6":
		switch_user()
	case "X", "x":
		os.Exit(0)
	default:
		fmt.Println("Invalid option")
		time.Sleep(1 * time.Second)
		main_menu()
	}
}

func connect_trading_ws() error {
	ws_mutex.Lock()
	defer ws_mutex.Unlock()

	if trading_ws != nil {
		trading_ws.Close()
		trading_ws = nil
	}

	ws_url := url.URL{Scheme: "ws", Host: "localhost:8080", Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(ws_url.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to connect to trading WebSocket: %v", err)
	}
	trading_ws = conn
	fmt.Printf("[WS] Connected to trading server\n")
	return nil
}

func close_trading_ws() {
	ws_mutex.Lock()
	defer ws_mutex.Unlock()

	if trading_ws != nil {
		trading_ws.Close()
		trading_ws = nil
		fmt.Printf("[WS] Closed trading connection\n")
	}
}

func send_trading_message(message map[string]interface{}) (*TradeResponse, error) {
	if err := connect_trading_ws(); err != nil {
		return nil, fmt.Errorf("connection failed: %v", err)
	}

	defer close_trading_ws()

	message["token"] = token

	fmt.Printf("[WS] Sending message: %v\n", message["type"])

	if err := trading_ws.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("failed to set write deadline: %v", err)
	}

	if err := trading_ws.WriteJSON(message); err != nil {
		return nil, fmt.Errorf("failed to send message: %v", err)
	}

	if err := trading_ws.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("failed to set read deadline: %v", err)
	}

	var response TradeResponse
	if err := trading_ws.ReadJSON(&response); err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	fmt.Printf("[WS] Received response: %s\n", response.Status)
	return &response, nil
}

func create_new_user() {
	clearScreen()
	fmt.Println("=== CREATE NEW USER ===")

	var username string
	fmt.Print("Enter username (3-30 chars, lowercase + numbers only): ")
	fmt.Scanln(&username)

	if username == "" {
		fmt.Println("[!] Username cannot be empty")
		enter_to_continue()
		return
	}

	reqBody := fmt.Sprintf(`{"username":"%s"}`, username)
	resp, err := http.Post("http://localhost:8080/api/create", "application/json", strings.NewReader(reqBody))
	if err != nil {
		fmt.Printf("[!] Error: %v\n", err)
		enter_to_continue()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var createResp CreateUserResponse
	json.Unmarshal(body, &createResp)

	if createResp.Status == "success" {
		newToken := createResp.Account["token"].(string)
		fmt.Printf("[+] User created successfully!\n")
		fmt.Printf("[~] Your new token: %s\n", newToken)
		fmt.Printf("[~] Starting balance: 200 SIGE\n\n")

		var useNewToken string
		fmt.Print("Use this token now? (y/n): ")
		fmt.Scanln(&useNewToken)

		if strings.ToLower(useNewToken) == "y" {
			token = newToken
			fmt.Println("[+] Switched to new account!")
		}
	} else {
		fmt.Printf("[!] Error: %s\n", createResp.Message)
	}

	enter_to_continue()
}

func check_assets() {
	clearScreen()
	fmt.Println("=== MY ASSETS ===")

	if token == "" {
		fmt.Println("[!] Please login first")
		enter_to_continue()
		return
	}

	reqBody := fmt.Sprintf(`{"id":"%s"}`, token)
	resp, err := http.Post("http://localhost:8080/api/assets", "application/json", strings.NewReader(reqBody))
	if err != nil {
		fmt.Printf("[!] Error: %v\n", err)
		enter_to_continue()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var assetResp AssetResponse
	json.Unmarshal(body, &assetResp)

	if assetResp.Status == "success" {
		fmt.Printf("[+] Account: %s\n\n", token[:8]+"...")
		fmt.Printf("%-15s %s\n", "ASSET", "AMOUNT")
		fmt.Println(strings.Repeat("=", 30))

		assetMap := make(map[string]float64)
		for _, asset := range assetResp.Assets {
			assetName := asset["asset_name"].(string)
			amount := asset["amount"].(float64)
			assetMap[assetName] += amount
		}

		assetNames := make([]string, 0, len(assetMap))
		for name := range assetMap {
			assetNames = append(assetNames, name)
		}
		sort.Strings(assetNames)

		if len(assetNames) == 0 {
			fmt.Println("No assets found")
		} else {
			for _, name := range assetNames {
				fmt.Printf("%-15s %.8f\n", name, assetMap[name])
			}
		}
	} else {
		fmt.Printf("[!] Error: %s\n", assetResp.Message)
	}

	enter_to_continue()
}

func place_limit_order() {
	clearScreen()
	fmt.Println("=== PLACE LIMIT ORDER ===")

	if token == "" {
		fmt.Println("[!] Please login first")
		enter_to_continue()
		return
	}

	var orderType, ticker, amountStr, priceStr string

	fmt.Print("Order type (BUY/SELL): ")
	fmt.Scanln(&orderType)
	orderType = strings.ToUpper(orderType)

	if orderType != "BUY" && orderType != "SELL" {
		fmt.Println("[!] Invalid order type. Use BUY or SELL")
		enter_to_continue()
		return
	}

	fmt.Print("Ticker (e.g., SIGE/HACK): ")
	fmt.Scanln(&ticker)

	parts := strings.Split(ticker, "/")
	if len(parts) != 2 {
		fmt.Println("[!] Invalid ticker format. Use format like SIGE/HACK")
		enter_to_continue()
		return
	}

	if orderType == "BUY" {
		fmt.Printf("===>>> You are BUYING %s with %s\n", parts[0], parts[1])
	} else {
		fmt.Printf("===>>> You are SELLING %s for %s\n", parts[0], parts[1])
	}

	fmt.Print("Amount: ")
	fmt.Scanln(&amountStr)
	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil || amount <= 0 {
		fmt.Println("[!] Invalid amount")
		enter_to_continue()
		return
	}

	fmt.Print("Price: ")
	fmt.Scanln(&priceStr)
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil || price <= 0 {
		fmt.Println("[!] Invalid price")
		enter_to_continue()
		return
	}

	fmt.Printf("\n[~] Order Summary:\n")
	fmt.Printf("Type: %s\nTicker: %s\nAmount: %.8f\nPrice: %.8f\n\n", orderType, ticker, amount, price)

	var confirm string
	fmt.Print("Confirm order? (y/n): ")
	fmt.Scanln(&confirm)

	if strings.ToLower(confirm) != "y" {
		fmt.Println("[!] Order cancelled")
		enter_to_continue()
		return
	}

	message := map[string]interface{}{
		"type":       "PLACE",
		"order_type": orderType,
		"amount":     amount,
		"ticker":     ticker,
		"price":      price,
	}

	fmt.Println("[~] Placing order...")
	response, err := send_trading_message(message)
	if err != nil {
		fmt.Printf("[!] Error: %v\n", err)
	} else if response.Status == "success" {
		fmt.Printf("[+] %s\n", response.Message)
	} else {
		fmt.Printf("[!] %s\n", response.Message)
	}

	enter_to_continue()
}

func execute_market_order() {
	clearScreen()
	fmt.Println("=== EXECUTE MARKET ORDER ===")

	if token == "" {
		fmt.Println("[!] Please login first")
		enter_to_continue()
		return
	}

	var orderType, ticker, amountStr, slippageStr string

	fmt.Print("Order type (BUY/SELL): ")
	fmt.Scanln(&orderType)
	orderType = strings.ToUpper(orderType)

	if orderType != "BUY" && orderType != "SELL" {
		fmt.Println("[!] Invalid order type. Use BUY or SELL")
		enter_to_continue()
		return
	}

	fmt.Print("Ticker (e.g., SIGE/HACK): ")
	fmt.Scanln(&ticker)

	parts := strings.Split(ticker, "/")
	if len(parts) != 2 {
		fmt.Println("[!] Invalid ticker format. Use format like SIGE/HACK")
		enter_to_continue()
		return
	}

	if orderType == "BUY" {
		fmt.Printf("===>>> You are BUYING %s with %s\n", parts[0], parts[1])
	} else {
		fmt.Printf("===>>> You are SELLING %s for %s\n", parts[0], parts[1])
	}

	fmt.Printf("\n[i] Note: Market orders execute at the best available price\n")
	fmt.Printf("[i] This may be different from the estimated price!\n") //but ive set a slippage so idc what the user does

	fmt.Print("Amount: ")
	fmt.Scanln(&amountStr)
	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil || amount <= 0 {
		fmt.Println("[!] Invalid amount")
		enter_to_continue()
		return
	}

	fmt.Print("Max slippage % (default 2): ")
	fmt.Scanln(&slippageStr)
	slippage := 2.0
	if slippageStr != "" {
		slippage, err = strconv.ParseFloat(slippageStr, 64)
		if err != nil || slippage < 0 {
			slippage = 2.0
		}
	}

	fmt.Printf("\n# Market Order Summary:\n")
	fmt.Printf("Type: %s %s\nTicker: %s\nAmount: %.8f\nMax Slippage: %.1f%%\n\n",
		orderType, "MARKET", ticker, amount, slippage)
	fmt.Println("[!] Market orders execute immediately at current market prices!")

	var confirm string
	fmt.Print("Confirm market order? (y/n): ")
	fmt.Scanln(&confirm)

	if strings.ToLower(confirm) != "y" {
		fmt.Println("[!] Order cancelled")
		enter_to_continue()
		return
	}

	message := map[string]interface{}{
		"type":       "MARKET",
		"order_type": orderType,
		"amount":     amount,
		"ticker":     ticker,
		"slippage":   slippage,
	}

	fmt.Println("[~] Executing market order...")

	response, err := send_trading_message(message)
	if err != nil {
		fmt.Printf("[!] Error: %v\n", err)
	} else if response.Status == "success" {
		fmt.Printf("[+] %s\n", response.Message)
	} else {
		fmt.Printf("[!] %s\n", response.Message)
	}

	enter_to_continue()
}

func switch_user() {
	clearScreen()
	fmt.Println("=== SWITCH USER ===")

	var newToken string
	fmt.Printf("Current token: %s\n\n", token[:8]+"...")
	fmt.Print("Enter new token: ")
	fmt.Scanln(&newToken)

	if newToken == "" {
		fmt.Println("[!] Token cannot be empty")
		enter_to_continue()
		return
	}

	reqBody := fmt.Sprintf(`{"id":"%s"}`, newToken)
	resp, err := http.Post("http://localhost:8080/api/assets", "application/json", strings.NewReader(reqBody))
	if err != nil {
		fmt.Printf("[!] Error testing token: %v\n", err)
		enter_to_continue()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var assetResp AssetResponse
	json.Unmarshal(body, &assetResp)

	if assetResp.Status == "success" {
		token = newToken
		fmt.Printf("[+] Successfully switched to new account!\n")
		fmt.Printf("[+] New token: %s\n", token[:8]+"...")
	} else {
		fmt.Printf("[!] Invalid token or error: %s\n", assetResp.Message)
	}

	enter_to_continue()
}

func main() {
	clearScreen()
	fmt.Println("//  SIEGE CT TRADING TERMINAL")
	fmt.Println("=============================")

	if len(os.Args) > 1 && os.Args[1] != "" {
		token = os.Args[1]
		fmt.Printf("Using token from arguments: %s\n", token[:8]+"...")
	} else {
		fmt.Println("Enter your token to login (or press ENTER to create a new user):")
		fmt.Print("TOKEN> ")
		fmt.Scanln(&token)

		if token == "" {
			create_new_user()
		}
	}

	fmt.Println("\n[+] Welcome to SIEGE CT (Coin Trading)!")
	fmt.Println("[!] Tip: Dont take risky trades!!!")
	time.Sleep(2 * time.Second)

	for {
		main_menu()
		var option string
		fmt.Scanln(&option)
		handle_option(option)
	}
}
