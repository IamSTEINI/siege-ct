package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	_ "modernc.org/sqlite"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}
var ctx context.Context = context.Background()
var db_file = "siege-ct.db"

var SQL_orders_table = `CREATE TABLE IF NOT EXISTS orders (
id TEXT PRIMARY KEY,
order_type TEXT,
amount REAL,
ticker TEXT,
price REAL,
token TEXT)`

var SQL_accounts_table = `CREATE TABLE IF NOT EXISTS accounts (
id TEXT PRIMARY KEY,
account_name TEXT,
account_token TEXT)`

var SQL_assets_table = `CREATE TABLE IF NOT EXISTS assets (
id TEXT PRIMARY KEY,
asset_name TEXT,
account_id TEXT,
asset_amount REAL)`

type ORDER struct {
	id, order_type string
	amount         float64
	ticker         string
	price          float64
	token          string
}

type TICKER_INFO struct {
	TICKER string  `json:"ticker"`
	VOLUME float64 `json:"volume"`
}

type ACCOUNT struct {
	id, account_name, account_token string
}

type SQL_QUEUE struct {
	action   string
	order    *ORDER
	username *string
	token    *string
	asset    *string
	id       *string
	amount   *float64
	response chan interface{}
}

type REDIS_QUEUE struct {
	action     string
	order      *ORDER
	response   chan interface{}
	ticker     *string
	id         *string
	token      *string
	order_type *string
	asset      *string
	amount     *float64
}

type ORDER_QUEUE struct {
	order ORDER
}

var REDIS_ORDER_QUEUE = make(chan REDIS_QUEUE, 5000)
var SQL_ORDER_QUEUE = make(chan SQL_QUEUE, 5000)
var ORDER_FULFILL_QUEUE = make(chan ORDER_QUEUE, 5000)
var DEFAULT_ASSET_NAME string = "SIGE" // Nope this is not a spelling mistake, tickers are 4 letters long...
var DEFAULT_ASSET_AMOUNT float64 = 200

// ? CALCULATIONS
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

func get_current_market_price(ticker string) (float64, error) {
	log.Printf("[MARKET PRICE] Getting current market price for %s", ticker)

	orderbook_buy, err := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "BUY")
	if err != nil {
		log.Printf("[MARKET PRICE ERROR] Failed to get buy orderbook for %s: %v", ticker, err)
		return 0, err
	}

	orderbook_sell, err := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "SELL")
	if err != nil {
		log.Printf("[MARKET PRICE ERROR] Failed to get sell orderbook for %s: %v", ticker, err)
		return 0, err
	}

	buy_orders_map := make(map[float64]float64)
	if buy_orders, ok := orderbook_buy.([]map[string]string); ok {
		for _, order := range buy_orders {
			price, err := strconv.ParseFloat(order["price"], 64)
			if err != nil {
				continue
			}
			amount, err := strconv.ParseFloat(order["amount"], 64)
			if err != nil {
				continue
			}
			buy_orders_map[price] += amount
		}
	}

	sell_orders_map := make(map[float64]float64)
	if sell_orders, ok := orderbook_sell.([]map[string]string); ok {
		for _, order := range sell_orders {
			price, err := strconv.ParseFloat(order["price"], 64)
			if err != nil {
				continue
			}
			amount, err := strconv.ParseFloat(order["amount"], 64)
			if err != nil {
				continue
			}
			sell_orders_map[price] += amount
		}
	}

	buys_vwap := vwap(buy_orders_map, 5, true)
	sells_vwap := vwap(sell_orders_map, 5, false)

	var market_price float64
	if buys_vwap > 0 && sells_vwap > 0 {
		market_price = (buys_vwap + sells_vwap) / 2
		log.Printf("[MARKET PRICE] %s mid price: %.8f (bid: %.8f, ask: %.8f)", ticker, market_price, buys_vwap, sells_vwap)
	} else if sells_vwap > 0 {
		market_price = sells_vwap
		log.Printf("[MARKET PRICE] %s using ask price: %.8f", ticker, market_price)
	} else if buys_vwap > 0 {
		market_price = buys_vwap
		log.Printf("[MARKET PRICE] %s using bid price: %.8f", ticker, market_price)
	} else {
		log.Printf("[MARKET PRICE ERROR] No liquidity available for %s", ticker)
		return 0, fmt.Errorf("no liquidity available for ticker %s", ticker)
	}

	return market_price, nil
}

// * SYSTEM

func order_fulfiller_worker(queue <-chan ORDER_QUEUE) {
	log.Println("[ORDER FULFILLER] Worker started and waiting for orders...")
	for job := range queue {
		log.Printf("[ORDER FULFILLER] Received order: %+v", job.order)
		var account_token string = job.order.token
		var ticker string = job.order.ticker
		var price float64 = job.order.price

		if price == -1 { // MARKET ORDER
			orderbook_buy, err := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "BUY")
			if err != nil {
				log.Println("[ERROR] [ORDER FULFILL]", err)
				continue
			}

			orderbook_sell, err1 := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "SELL")
			if err1 != nil {
				log.Println("[ERROR] [ORDER FULFILL]", err1)
				continue
			}

			buy_orders_map := make(map[float64]float64)
			if buy_orders, ok := orderbook_buy.([]map[string]string); ok {
				log.Printf("[MARKET ORDER] Found %d buy orders for VWAP calculation", len(buy_orders))
				for _, order := range buy_orders {
					price, err := strconv.ParseFloat(order["price"], 64)
					if err != nil {
						continue
					}
					amount, err := strconv.ParseFloat(order["amount"], 64)
					if err != nil {
						continue
					}
					buy_orders_map[price] += amount
					log.Printf("[MARKET ORDER] Buy order: %.8f @ %.8f", amount, price)
				}
			}

			sell_orders_map := make(map[float64]float64)
			if sell_orders, ok := orderbook_sell.([]map[string]string); ok {
				log.Printf("[MARKET ORDER] Found %d sell orders for VWAP calculation", len(sell_orders))
				for _, order := range sell_orders {
					price, err := strconv.ParseFloat(order["price"], 64)
					if err != nil {
						continue
					}
					amount, err := strconv.ParseFloat(order["amount"], 64)
					if err != nil {
						continue
					}
					sell_orders_map[price] += amount
					log.Printf("[MARKET ORDER] Sell order: %.8f @ %.8f", amount, price)
				}
			}

			buys_vwap := vwap(buy_orders_map, 10, true)    // TOP n SAME AS IN TERMINAL
			sells_vwap := vwap(sell_orders_map, 10, false) // TOP n SAME AS IN TERMINAL

			log.Printf("[MARKET ORDER] VWAP calculation: buys=%.8f, sells=%.8f", buys_vwap, sells_vwap)

			if buys_vwap > 0 && sells_vwap > 0 {
				price = (buys_vwap + sells_vwap) / 2
				log.Printf("[MARKET ORDER] Using average VWAP: %.8f", price)
			} else if sells_vwap > 0 {
				price = sells_vwap
				log.Printf("[MARKET ORDER] Using sell VWAP: %.8f", price)
			} else if buys_vwap > 0 {
				price = buys_vwap
				log.Printf("[MARKET ORDER] Using buy VWAP: %.8f", price)
			} else {
				log.Printf("[ERROR] [ORDER FULFILL] No liquidity available for market order")
				continue
			}
		}

		ticker_parts := strings.Split(strings.Replace(ticker, "-", "/", -1), "/")
		if len(ticker_parts) != 2 {
			log.Printf("[ERROR] [ORDER FULFILL] Invalid ticker format: %s", ticker)
			continue
		}
		base_asset := ticker_parts[0]
		quote_asset := ticker_parts[1]

		var asset_to_check string
		var amount_to_check float64
		if job.order.order_type == "BUY" {
			asset_to_check = quote_asset
			amount_to_check = job.order.amount * price * 1.02 // MARGIN for exchange security
		} else {
			asset_to_check = base_asset
			amount_to_check = job.order.amount
		}

		log.Printf("[ORDER FULFILLER] %s order requires %.8f %s (base: %s, quote: %s)",
			job.order.order_type, amount_to_check, asset_to_check, base_asset, quote_asset)

		var eligible, err = check_user_assets(asset_to_check, amount_to_check, account_token)
		if err != nil {
			log.Println("[ERROR] [OAC]", err)
			continue
		}
		if !eligible {
			log.Printf("[ERROR] [ORDER FULFILL] Insufficient funds for order fulfillment")
			continue
		}

		log.Printf("[ORDER FULFILLER] Processing market %s order for %s: %.8f %s at estimated price %.8f",
			job.order.order_type, account_token, job.order.amount, ticker, price)

		if job.order.order_type == "BUY" {
			reserve_amount := job.order.amount * price * 1.02
			err = reserve_user_assets(quote_asset, reserve_amount, account_token)
			if err != nil {
				log.Printf("[ORDER FULFILLER ERROR] Failed to reserve assets: %v", err)
				continue
			}
			err = execute_market_order(&job.order, price, "SELL", account_token)
			if err != nil {
				release_reservation(quote_asset, reserve_amount, account_token)
				log.Printf("[ORDER FULFILLER ERROR] Failed to execute market order: %v", err)
				continue
			}
			release_reservation(quote_asset, reserve_amount, account_token)
		} else {
			err = reserve_user_assets(base_asset, job.order.amount, account_token)
			if err != nil {
				log.Printf("[ORDER FULFILLER ERROR] Failed to reserve assets: %v", err)
				continue
			}

			err = execute_market_order(&job.order, price, "BUY", account_token)
			if err != nil {
				release_reservation(base_asset, job.order.amount, account_token)
				log.Printf("[ORDER FULFILLER ERROR] Failed to execute market order: %v", err)
				continue
			}

			release_reservation(base_asset, job.order.amount, account_token)
		}

		log.Printf("[ORDER FULFILLER] Completed market order for %s", account_token)
	}
}

func limit_order_manager(sql_driver *sql.DB, ctx context.Context, redis_client *redis.Client) {
	log.Println("[LIMIT ORDER MANAGER] Starting limit order monitoring...")

	ticker_timer := time.NewTicker(8 * time.Second)
	defer ticker_timer.Stop()

	for {
		select {
		case <-ticker_timer.C:
			log.Printf("[LIMIT ORDER MANAGER] Checking limit orders...")

			orders, err := get_all_orders_from_sql(sql_driver)
			if err != nil {
				log.Printf("[LIMIT ORDER MANAGER ERROR] Failed to get orders: %v", err)
				continue
			}

			market_prices := make(map[string]float64)
			orders_to_process := []ORDER{}
			for orders.Next() {
				var order ORDER
				err := orders.Scan(&order.id, &order.order_type, &order.amount, &order.ticker, &order.price, &order.token)
				if err != nil {
					log.Printf("[LIMIT ORDER MANAGER ERROR] Failed to scan order: %v", err)
					continue
				}

				if order.price == -1 {
					continue
				}

				orders_to_process = append(orders_to_process, order)
			}
			orders.Close()

			if len(orders_to_process) == 0 {
				log.Printf("[LIMIT ORDER MANAGER] No limit orders to monitor")
				continue
			}

			log.Printf("[LIMIT ORDER MANAGER] Found %d limit orders to monitor", len(orders_to_process))

			for _, order := range orders_to_process {
				ticker_key := strings.ToUpper(order.ticker)

				if _, exists := market_prices[ticker_key]; !exists {
					market_price, err := get_current_market_price(order.ticker)
					if err != nil {
						log.Printf("[LIMIT ORDER MANAGER] Failed to get market price for %s: %v", ticker_key, err)
						market_prices[ticker_key] = -1
						continue
					}
					market_prices[ticker_key] = market_price
				}
			}

			for _, order := range orders_to_process {
				ticker_key := strings.ToUpper(order.ticker)
				market_price, exists := market_prices[ticker_key]

				if !exists || market_price == -1 {
					continue
				}

				should_execute := false

				if order.order_type == "BUY" {
					if market_price <= order.price {
						should_execute = true
						log.Printf("[LIMIT ORDER TRIGGER] BUY order %s: market price %.8f <= limit price %.8f",
							order.id, market_price, order.price)
					}
				} else if order.order_type == "SELL" {
					if market_price >= order.price {
						should_execute = true
						log.Printf("[LIMIT ORDER TRIGGER] SELL order %s: market price %.8f >= limit price %.8f",
							order.id, market_price, order.price)
					}
				}

				if should_execute {
					log.Printf("[LIMIT ORDER EXECUTION] Converting limit order %s (%s %.8f %s @ %.8f) to market order",
						order.id, order.order_type, order.amount, order.ticker, order.price)

					go func(orderToDelete ORDER) {
						redis_response := make(chan interface{}, 1)
						REDIS_ORDER_QUEUE <- REDIS_QUEUE{
							action:   "CANCEL-ORDER",
							id:       &orderToDelete.id,
							response: redis_response,
						}
						<-redis_response

						sql_response := make(chan interface{}, 1)
						SQL_ORDER_QUEUE <- SQL_QUEUE{
							action:   "CANCEL-ORDER",
							id:       &orderToDelete.id,
							token:    &orderToDelete.token,
							response: sql_response,
						}
						<-sql_response

						log.Printf("[LIMIT ORDER CLEANUP] Removed limit order %s from database", orderToDelete.id)
					}(order)

					market_order := ORDER{
						id:         uuid.NewString(),
						order_type: order.order_type,
						amount:     order.amount,
						ticker:     order.ticker,
						price:      -1,
						token:      order.token,
					}

					select {
					case ORDER_FULFILL_QUEUE <- ORDER_QUEUE{order: market_order}:
						log.Printf("[LIMIT ORDER EXECUTED] Submitted market order %s (converted from limit order %s)",
							market_order.id, order.id)
					default:
						log.Printf("[LIMIT ORDER ERROR] Order fulfillment queue is full, skipping market order %s", market_order.id)
					}
				}
			}

		case <-ctx.Done():
			log.Println("[LIMIT ORDER MANAGER] Shutting down...")
			return
		}
	}
}

func redis_queue_worker(sql_driver *sql.DB, queue <-chan REDIS_QUEUE, ctx context.Context, redis_client *redis.Client) {
	for job := range queue {
		switch job.action {
		case "PLACE":
			log.Println("[REDIS QUEUE WORKER] Processing order #", job.order.id)
			order, err := create_redis_order(ctx, redis_client, job.order.id, job.order.order_type, job.order.amount, strings.ToUpper(job.order.ticker), job.order.price, job.order.token)
			if err != nil {
				log.Println("[ERROR] [REDIS QUEUE WORKER]", err)
				job.response <- err
				continue
			}
			job.response <- order
		case "GET-ORDERBOOK":
			log.Println("[REDIS QUEUE WORKER] Processing GET-ORDERBOOK")
			orderTypeKey := strings.ToLower(*job.order_type)
			key := fmt.Sprintf("orderbook:%s:%s", *job.ticker, orderTypeKey)
			log.Println("[REDIS QUEUE WORKER] Looking for key:", key)
			res, err := redis_client.ZRangeWithScores(ctx, key, 0, -1).Result()
			if err != nil {
				log.Println("[ERROR] [REDIS QUEUE WORKER]", err)
				job.response <- err
				continue
			}
			var orders []map[string]string
			for _, z := range res {
				orderKey := fmt.Sprintf("order:%v", z.Member)
				orderData, err := redis_client.HGetAll(ctx, orderKey).Result()
				if err != nil {
					log.Println("Error fetching hash for", z.Member, err)
					continue
				}
				orderData["score"] = fmt.Sprintf("%f", z.Score)
				orderData["token"] = ""
				orders = append(orders, orderData)
			}

			job.response <- orders
		case "CANCEL-ORDER":
			delete_redis_order(ctx, redis_client, *job.id)
			job.response <- "Order deleted from Redis"
		case "RESERVE-ASSET":
			reservation_key := fmt.Sprintf("reservation:%s:%s", *job.token, *job.asset)
			current, err := redis_client.Get(ctx, reservation_key).Float64()
			if err != nil && err != redis.Nil {
				job.response <- err
				continue
			}

			new_reservation := current + *job.amount
			err = redis_client.Set(ctx, reservation_key, new_reservation, 0).Err()
			if err != nil {
				job.response <- err
				continue
			}

			log.Printf("[REDIS QUEUE WORKER] Reserved %.8f %s for user %s (total reserved: %.8f)", *job.amount, *job.asset, *job.token, new_reservation)
			job.response <- true
		case "RELEASE-RESERVATION":
			reservation_key := fmt.Sprintf("reservation:%s:%s", *job.token, *job.asset)
			current, err := redis_client.Get(ctx, reservation_key).Float64()
			if err != nil && err != redis.Nil {
				job.response <- err
				continue
			}

			newReservation := current - *job.amount
			if newReservation <= 0 {
				err = redis_client.Del(ctx, reservation_key).Err()
			} else {
				err = redis_client.Set(ctx, reservation_key, newReservation, 0).Err()
			}

			if err != nil {
				job.response <- err
				continue
			}

			log.Printf("[REDIS QUEUE WORKER] Released %.8f %s for user %s (remaining reserved: %.8f)", *job.amount, *job.asset, *job.token, newReservation)
			job.response <- true
		case "UPDATE-ORDER":
			order_key := fmt.Sprintf("order:%s", *job.id)
			err := redis_client.HSet(ctx, order_key, "amount", fmt.Sprintf("%f", *job.amount)).Err()
			if err != nil {
				log.Printf("[REDIS QUEUE WORKER] Failed to update order %s: %v", *job.id, err)
				job.response <- err
				continue
			}

			log.Printf("[REDIS QUEUE WORKER] Updated order %s amount to %.8f", *job.id, *job.amount)
			job.response <- true
		}
	}
}

func sql_queue_worker(sql_driver *sql.DB, queue <-chan SQL_QUEUE) {
	for job := range queue {
		switch job.action {
		case "PLACE":
			log.Println("[SQL QUEUE WORKER] Processing order #", job.order.id)
			order, err := insert_order_into_sql(sql_driver, job.order.order_type, job.order.amount, strings.ToUpper(job.order.ticker), job.order.price, job.order.token, job.order.id)
			if err != nil {
				log.Printf("[SQL QUEUE WORKER] Error inserting order %s into SQL: %v", job.order.id, err)
			}
			job.response <- order
		case "CREATE-USER":
			log.Println("[SQL QUEUE WORKER] Checking if username exists:", *job.username)
			var exists bool
			rows, err := sql_driver.Query("SELECT * FROM accounts WHERE account_name = ?", *job.username)
			if err != nil {
				job.response <- err
				continue
			}
			exists = rows.Next()
			rows.Close()

			if exists {
				job.response <- true
				continue
			}

			newToken := uuid.NewString()
			account := ACCOUNT{
				id:            uuid.NewString(),
				account_name:  *job.username,
				account_token: newToken,
			}

			_, err = sql_driver.Exec("INSERT INTO accounts (id, account_name, account_token) VALUES (?, ?, ?)",
				account.id, account.account_name, account.account_token)
			if err != nil {
				log.Printf("[SQL QUEUE WORKER] Error creating user %s: %v", *job.username, err)
				job.response <- err
				continue
			}

			log.Printf("[SQL QUEUE WORKER] Created new user: %s with token: %s", *job.username, newToken)
			job.response <- account
		case "DELETE-USER":
			var token = *job.token
			_, err := sql_driver.Exec("DELETE FROM accounts WHERE account_token = ?", token)
			if err != nil {
				job.response <- err
				continue
			}
			job.response <- true
		case "GET-ACCOUNT-ID-BY-TOKEN":
			var token string = *job.token
			var account_id string
			err := sql_driver.QueryRow("SELECT id FROM accounts WHERE account_token = ?", token).Scan(&account_id)
			if err == sql.ErrNoRows {
				job.response <- fmt.Errorf("account not found for token")
				continue
			} else if err != nil {
				job.response <- err
				continue
			}
			job.response <- account_id
		case "GET-TOKEN-BY-ORDER-ID":
			var order_id string = *job.id
			var token string
			err := sql_driver.QueryRow("SELECT token FROM orders WHERE id = ?", order_id).Scan(&token)
			if err == sql.ErrNoRows {
				job.response <- fmt.Errorf("order not found for ID")
				continue
			} else if err != nil {
				job.response <- err
				continue
			}
			job.response <- token
		case "ADD-ASSET":
			var asset string = *job.asset
			var account_id string = *job.id
			var asset_amount float64 = *job.amount
			id := uuid.NewString()
			_, err := sql_driver.Exec("INSERT INTO assets (id, asset_name, account_id, asset_amount) VALUES (?, ?, ?, ?)",
				id, asset, account_id, asset_amount)
			if err != nil {
				job.response <- err
				continue
			}
			job.response <- true
		case "GET-ALL-ASSETS":
			var account_id string = *job.id
			rows, err := sql_driver.Query("SELECT * FROM assets WHERE account_id = ?", account_id)
			if err != nil {
				job.response <- err
				continue
			}

			var assets []map[string]interface{}
			for rows.Next() {
				var id, asset_name, acc_id string
				var asset_amount float64

				if err := rows.Scan(&id, &asset_name, &acc_id, &asset_amount); err != nil {
					log.Printf("[SQL QUEUE WORKER] Error scanning asset row: %v", err)
					continue
				}

				asset := map[string]interface{}{
					"id":         id,
					"asset_name": asset_name,
					"account_id": acc_id,
					"amount":     asset_amount,
				}
				assets = append(assets, asset)
			}

			if err := rows.Err(); err != nil {
				job.response <- err
				continue
			}

			rows.Close()
			job.response <- assets
		case "REMOVE-ASSETS":
			var asset string = *job.asset
			var account_id string = *job.id
			var asset_amount float64 = *job.amount

			var current_amount float64
			err := sql_driver.QueryRow("SELECT asset_amount FROM assets WHERE account_id = ? AND asset_name = ?", account_id, asset).Scan(&current_amount)
			if err == sql.ErrNoRows {
				job.response <- fmt.Errorf("asset %s not found for account %s", asset, account_id)
				continue
			} else if err != nil {
				job.response <- err
				continue
			}

			if current_amount < asset_amount {
				job.response <- fmt.Errorf("insufficient %s balance: have %.8f, need %.8f", asset, current_amount, asset_amount)
				continue
			}

			new_amount := current_amount - asset_amount
			if new_amount == 0 {
				_, err = sql_driver.Exec("DELETE FROM assets WHERE account_id = ? AND asset_name = ?", account_id, asset)
			} else {
				_, err = sql_driver.Exec("UPDATE assets SET asset_amount = ? WHERE account_id = ? AND asset_name = ?", new_amount, account_id, asset)
			}

			if err != nil {
				job.response <- err
				continue
			}

			job.response <- true
		case "GET-ALL-TICKERS":
			rows, err := sql_driver.Query("SELECT * FROM assets")
			if err != nil {
				job.response <- err
				continue
			}
			var tickers []TICKER_INFO
			var assets []string
			// WE COMBINE EVERY TICKER WITH EVERY OTHER TICKER, Results in an exponential amount of trading pairs
			for rows.Next() {
				var asset_name string
				var id, account_id string
				var asset_amount float64
				if err := rows.Scan(&id, &asset_name, &account_id, &asset_amount); err != nil {
					log.Printf("[SQL QUEUE WORKER] Error scanning asset row: %v", err)
					continue
				}
				assets = append(assets, asset_name)
			}
			for _, asset := range assets {
				for _, asset_pair := range assets {
					if asset != asset_pair {
						var ticker = asset + "/" + asset_pair
						volume, err := get_volume(ticker)
						if err != nil {
							log.Printf("[SQL QUEUE WORKER] Error fetching volume for ticker %v: %v", ticker, err)
							job.response <- err
						}
						tickerExists := false
						for _, existingTicker := range tickers {
							if existingTicker.TICKER == ticker {
								tickerExists = true
								break
							}
						}

						if !tickerExists {
							tickers = append(tickers, TICKER_INFO{
								TICKER: ticker,
								VOLUME: volume,
							})
						}
					}
				}
			}
			job.response <- tickers
		case "UPDATE-ORDER-AMOUNT":
			var id string = *job.id
			var new_amount float64 = *job.amount

			result, err := sql_driver.Exec("UPDATE orders SET amount = ? WHERE id = ?", new_amount, id)
			if err != nil {
				job.response <- err
				continue
			}

			rowsAffected, _ := result.RowsAffected()
			if rowsAffected == 0 {
				job.response <- fmt.Errorf("order %s not found for update", id)
				continue
			}

			log.Printf("[SQL QUEUE WORKER] Updated order %s amount to %.8f", id, new_amount)
			job.response <- true
		case "CANCEL-ORDER":
			// DELETE FROM sql db
			var id string = *job.id
			var token string = *job.token

			var exists bool
			row := sql_driver.QueryRow("SELECT 1 FROM orders WHERE id = ? AND token = ?", id, token)
			err := row.Scan(&exists)
			if err == sql.ErrNoRows {
				log.Printf("[SQL QUEUE WORKER] Order %s not found or token mismatch", id)
				job.response <- false
				continue
			} else if err != nil {
				job.response <- err
				continue
			}

			result, err := sql_driver.Exec("DELETE FROM orders WHERE id = ? AND token = ?", id, token)
			if err != nil {
				job.response <- err
				continue
			}

			rowsAffected, _ := result.RowsAffected()
			if rowsAffected == 0 {
				job.response <- false
				continue
			}

			redis_response := make(chan interface{}, 1)
			REDIS_ORDER_QUEUE <- REDIS_QUEUE{
				action:   "CANCEL-ORDER",
				id:       &id,
				response: redis_response,
			}

			redis_result := <-redis_response
			if err, ok := redis_result.(error); ok {
				log.Printf("[SQL QUEUE WORKER] Failed to cancel order in Redis: %v", err)
				job.response <- false
			}

			job.response <- true
		}

	}
}

func get_volume(ticker string) (float64, error) {
	orderbook_buy, err := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "BUY")
	if err != nil {
		log.Println("[ERROR] [TICKER ACTIONS]", err)
		return 0, err
	}

	orderbook_sell, err1 := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "SELL")
	if err1 != nil {
		log.Println("[ERROR] [TICKER ACTIONS]", err1)
		return 0, err1
	}

	var totalVolume float64 = 0

	if buyOrders, ok := orderbook_buy.([]map[string]string); ok {
		for _, order := range buyOrders {
			amount, err := strconv.ParseFloat(order["amount"], 64)
			if err != nil {
				log.Printf("[ERROR] Failed to parse amount in buy order: %v", err)
				continue
			}
			totalVolume += amount
		}
	}

	if sellOrders, ok := orderbook_sell.([]map[string]string); ok {
		for _, order := range sellOrders {
			amount, err := strconv.ParseFloat(order["amount"], 64)
			if err != nil {
				log.Printf("[ERROR] Failed to parse amount in sell order: %v", err)
				continue
			}
			totalVolume += amount
		}
	}

	return totalVolume, nil
}

func get_orderbook(ticker string, order_type string) (interface{}, error) {
	response := make(chan interface{}, 1)
	REDIS_ORDER_QUEUE <- REDIS_QUEUE{
		action:     "GET-ORDERBOOK",
		ticker:     &ticker,
		order_type: &order_type,
		response:   response,
	}

	res := <-response

	if err, ok := res.(error); ok {
		return nil, err
	}

	return res, nil
}

// ? HTTP SERVER ROUTES

func handle_client_message(messageType int, message []byte) {
	log.Println("[MESSAGE HANDLER] [RECV]", string(message))
}
func handle_ticker_actions(messageType int, message []byte, conn *websocket.Conn, ticker string) {
	log.Println("[TICKER ACTIONS] [RECV]", string(message))

	switch string(message) {
	case "GET-ORDERBOOK":
		log.Println("[TICKER REQUESTED] ", strings.Replace(ticker, "-", "/", -1))
		orderbook_buy, err := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "BUY")
		orderbook_sell, err1 := get_orderbook(strings.ToUpper(strings.Replace(ticker, "-", "/", -1)), "SELL")
		if err != nil {
			log.Println("[ERROR] [TICKER ACTIONS]", err)
		}
		if err1 != nil {
			log.Println("[ERROR] [TICKER ACTIONS]", err)
		}

		orderbook := map[string]interface{}{
			"orderbook": map[string]interface{}{
				"buy":  orderbook_buy,
				"sell": orderbook_sell,
			},
		}

		responseJSON, err := json.Marshal(orderbook)
		if err != nil {
			log.Println("[ERROR] [TICKER ACTIONS] Failed to marshal orderbook:", err)
			conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Internal server error"}`))
			return
		}
		conn.WriteMessage(websocket.TextMessage, responseJSON)
	default:
		conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing action"}`))
	}
}

func handle_client_action(messageType int, messageJson map[string]interface{}, conn *websocket.Conn) {
	log.Println("[ACTION HANDLER] [TYPE]", messageJson["type"])

	if token, ok := messageJson["token"].(string); !ok || token == "" {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Authentication required"}`))
		log.Println("[ACTION] Authentication error: No token provided")
		return
	}

	switch messageJson["type"] {
	case "PLACE":
		if amountVal, ok := messageJson["amount"]; ok {
			amount := amountVal.(float64)
			if ticker, ok := messageJson["ticker"].(string); ok {
				if priceVal, ok := messageJson["price"]; ok {
					price := priceVal.(float64)
					if price <= 0 {
						conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Price must be positive."}`))
						log.Printf("[ACTION] Invalid price: %f - Price must be positive", price)
						break
					}
					if order_type, ok := messageJson["order_type"].(string); ok {
						if order_type != "BUY" && order_type != "SELL" {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Invalid order_type."}`))
							log.Printf("[ACTION] Invalid order type: %s", order_type)
							break
						}
						token, _ := messageJson["token"].(string)

						order := ORDER{
							id:         uuid.NewString(),
							order_type: order_type,
							amount:     amount,
							ticker:     ticker,
							price:      price,
							token:      token,
						}

						redis_response := make(chan interface{}, 1)
						REDIS_ORDER_QUEUE <- REDIS_QUEUE{
							action:   "PLACE",
							order:    &order,
							response: redis_response,
						}

						redis_result := <-redis_response
						if err, ok := redis_result.(error); ok {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Failed to place order in Redis"}`))
							log.Printf("[ACTION] Failed to place order in Redis: %v", err)
							break
						}
						sql_response := make(chan interface{}, 1)
						SQL_ORDER_QUEUE <- SQL_QUEUE{
							action:   "PLACE",
							order:    &order,
							response: sql_response,
						}
						sql_result := <-sql_response
						if err, ok := sql_result.(error); ok {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Failed to place order in SQL"}`))
							log.Printf("[ACTION] Failed to place order in SQL: %v", err)
							break
						}
						orderDetails := map[string]interface{}{
							"id":         order.id,
							"order_type": order.order_type,
							"amount":     order.amount,
							"ticker":     order.ticker,
							"price":      order.price,
						}
						order_JSON, _ := json.Marshal(orderDetails)
						success_response := fmt.Sprintf(`{"status":"success","message":"Order placed","order":%s}`, string(order_JSON))
						conn.WriteMessage(websocket.TextMessage, []byte(success_response))
						log.Printf("[ACTION] %s order placed for %v %s at %v", order_type, amount, ticker, price)
					} else {
						conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid order_type"}`))
					}
				} else {
					conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid price"}`))
				}
			} else {
				conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid ticker"}`))
			}
		} else {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid amount"}`))
		}
	case "MARKET":
		if amountVal, ok := messageJson["amount"]; ok {
			amount := amountVal.(float64)
			if ticker, ok := messageJson["ticker"].(string); ok {
				if slippageVal, ok := messageJson["slippage"]; ok {
					slippage := int(slippageVal.(float64))
					if order_type, ok := messageJson["order_type"].(string); ok {
						if order_type != "BUY" && order_type != "SELL" {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Invalid order_type."}`))
							log.Printf("[ACTION] Invalid order type: %s", order_type)
							break
						}
						token, _ := messageJson["token"].(string)

						ticker_parts := strings.Split(strings.Replace(ticker, "-", "/", -1), "/")
						if len(ticker_parts) != 2 {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Invalid ticker format"}`))
							log.Printf("[MARKET ORDER] Invalid ticker format: %s", ticker)
							break
						}

						base_asset := ticker_parts[0]
						quote_asset := ticker_parts[1]

						var asset_to_check string
						var amount_to_check float64

						if order_type == "BUY" {
							asset_to_check = quote_asset
							amount_to_check = amount * 1.1
						} else {
							asset_to_check = base_asset
							amount_to_check = amount
						}

						log.Printf("[MARKET ORDER] Checking %s balance: %.8f %s needed for %s %s order",
							token[:8]+"...", amount_to_check, asset_to_check, order_type, ticker)

						eligible, err := check_user_assets(asset_to_check, amount_to_check, token)
						if err != nil {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Error while checking your assets"}`))
							log.Printf("[MARKET ORDER] Failed to check balance: %v", err)
							break
						}
						if eligible {
							log.Println("[MARKET ORDER] by ", token, " SLIPAGE: ", slippage, "AMOUNT:", amount, " @ ", ticker)

							market_order := ORDER{
								id:         uuid.NewString(),
								order_type: order_type,
								amount:     amount,
								ticker:     ticker,
								price:      -1, // Market order
								token:      token,
							}

							ORDER_FULFILL_QUEUE <- ORDER_QUEUE{
								order: market_order,
							}

							orderDetails := map[string]interface{}{
								"id":         market_order.id,
								"order_type": market_order.order_type,
								"amount":     market_order.amount,
								"ticker":     market_order.ticker,
								"type":       "MARKET",
							}
							order_JSON, _ := json.Marshal(orderDetails)
							success_response := fmt.Sprintf(`{"status":"success","message":"Market order submitted","order":%s}`, string(order_JSON))
							conn.WriteMessage(websocket.TextMessage, []byte(success_response))
						} else {
							conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Insufficient funds"}`))
						}
					} else {
						conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid order_type"}`))
					}
				} else {
					conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid slippage"}`))
				}
			} else {
				conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid ticker"}`))
			}
		} else {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Missing or invalid amount"}`))
		}
	case "TRANSFER":
		log.Println("[ACTION] TRANSFER not implemented yet")
	default:
		conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Invalid action"}`))
		log.Printf("[ACTION] Unknown action type: %v", messageJson["type"])
	}
	//BUY, SELL, TRANSFER, PLACE

	// WE ALSO NEED TO HANDLE SIMPLE MARKET ORDERS: BUY AND SELL, with a slipage. MARKET ENGINE gets the best price and buys the amount for the user
	// "A market order always buys only the requested amount, consuming existing limit orders piece by piece, so sellers often only sell part of their order until another buyer takes the rest."

}

func check_user_assets(asset string, amount float64, user_token string) (bool, error) {
	log.Printf("[UAC] Starting asset check for user token %s (asset: %s, amount: %.8f)", user_token[:8]+"...", asset, amount)

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ACCOUNT-ID-BY-TOKEN",
		token:    &user_token,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		log.Printf("[UAC ERROR] Failed to get account ID for token %s: %v", user_token[:8]+"...", err)
		return false, fmt.Errorf("failed to get account ID: %v", err)
	}

	accountId, ok := sql_result.(string)
	if !ok || accountId == "" {
		log.Printf("[UAC ERROR] Invalid account ID returned for token %s", user_token[:8]+"...")
		return false, fmt.Errorf("invalid account ID for token")
	}

	log.Printf("[UAC] Found account ID %s for token %s", accountId, user_token[:8]+"...")

	sql_response2 := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ALL-ASSETS",
		id:       &accountId,
		response: sql_response2,
	}

	sql_result2 := <-sql_response2
	if err, ok := sql_result2.(error); ok {
		log.Printf("[UAC ERROR] Failed to get assets for account %s: %v", accountId, err)
		return false, fmt.Errorf("failed to retrieve user assets: %v", err)
	}

	assets, ok := sql_result2.([]map[string]interface{})
	if !ok {
		log.Printf("[UAC ERROR] Invalid assets format returned for account %s", accountId)
		return false, fmt.Errorf("failed to parse user assets")
	}

	log.Printf("[UAC] Retrieved %d assets for account %s", len(assets), accountId)

	assetTotals := make(map[string]float64)

	for _, userAsset := range assets {
		assetName := userAsset["asset_name"].(string)
		assetAmount := userAsset["amount"].(float64)
		assetTotals[assetName] += assetAmount
	}

	i := 1
	for assetName, totalAmount := range assetTotals {
		log.Printf("[UAC] Asset %d: %s = %.8f (aggregated)", i, assetName, totalAmount)
		i++
	}

	if totalAmount, exists := assetTotals[asset]; exists {
		log.Printf("[UAC] Found matching asset %s: user has %.8f total, needs %.8f", asset, totalAmount, amount)
		if totalAmount >= amount {
			log.Printf("[UAC SUCCESS] User has sufficient %s balance: %.8f >= %.8f", asset, totalAmount, amount)
			return true, nil
		} else {
			log.Printf("[UAC FAIL] Insufficient %s balance: user has %.8f total, needs %.8f (shortfall: %.8f)",
				asset, totalAmount, amount, amount-totalAmount)
			return false, nil
		}
	}

	for _, userAsset := range assets {
		if assetName, exists := userAsset["asset_name"].(string); exists && strings.EqualFold(assetName, asset) {
			if assetAmount, exists := userAsset["amount"].(float64); exists {
				log.Printf("[UAC] Found matching asset %s: user has %.8f, needs %.8f", assetName, assetAmount, amount)
				if assetAmount >= amount {
					log.Printf("[UAC SUCCESS] User has sufficient %s balance: %.8f >= %.8f", assetName, assetAmount, amount)
					return true, nil
				} else {
					log.Printf("[UAC FAIL] Insufficient %s balance: user has %.8f, needs %.8f (shortfall: %.8f)",
						assetName, assetAmount, amount, amount-assetAmount)
					return false, nil
				}
			}
		}
	}

	log.Printf("[UAC FAIL] User does not have any %s assets", asset)
	return false, nil
}

func get_account_id_by_token(token string) (string, error) {
	log.Printf("[AUTH] Getting account ID for token %s...", token)

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ACCOUNT-ID-BY-TOKEN",
		token:    &token,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		return "", err
	}

	accountId, ok := sql_result.(string)
	if !ok || accountId == "" {
		return "", fmt.Errorf("invalid account ID for token")
	}

	log.Printf("[AUTH] Found account ID: %s for token %s", accountId, token)
	return accountId, nil
}

func get_token_by_order_id(order_id string) (string, error) {
	log.Printf("[AUTH] Getting token for order ID %s...", order_id)
	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-TOKEN-BY-ORDER-ID",
		id:       &order_id,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		return "", err
	}

	token, ok := sql_result.(string)
	if !ok || token == "" {
		return "", fmt.Errorf("invalid token for order ID")
	}

	log.Printf("[AUTH] Found token: %s for order ID %s", token, order_id)
	return token, nil
}

func reserve_user_assets(asset string, amount float64, token string) error {
	redis_response := make(chan interface{}, 1)
	REDIS_ORDER_QUEUE <- REDIS_QUEUE{
		action:   "RESERVE-ASSET",
		token:    &token,
		asset:    &asset,
		amount:   &amount,
		response: redis_response,
	}

	redis_result := <-redis_response
	if err, ok := redis_result.(error); ok {
		return fmt.Errorf("failed to reserve assets in Redis: %v", err)
	}

	log.Printf("[RESERVE] Reserved %.8f %s for user %s", amount, asset, token)
	return nil
}

func release_reservation(asset string, amount float64, token string) error {
	redis_response := make(chan interface{}, 1)
	REDIS_ORDER_QUEUE <- REDIS_QUEUE{
		action:   "RELEASE-RESERVATION",
		token:    &token,
		asset:    &asset,
		amount:   &amount,
		response: redis_response,
	}

	redis_result := <-redis_response
	if err, ok := redis_result.(error); ok {
		return fmt.Errorf("failed to release reservation in Redis: %v", err)
	}

	log.Printf("[RELEASE] Released %.8f %s for user %s", amount, asset, token)
	return nil
}

func execute_trade(buyer_token string, seller_token string, base_asset string, quote_asset string,
	amount float64, price float64, order_id string) error {

	log.Printf("===TRANSACTION #%s ====", order_id)
	log.Printf("[TRADE] Executing trade: %s buying %.8f %s at %.8f %s per unit from %s", buyer_token, amount, base_asset, price, quote_asset, seller_token)

	buyer_id, err := get_account_id_by_token(buyer_token)
	if err != nil {
		return fmt.Errorf("failed to get buyer account ID: %v", err)
	}

	seller_id, err := get_account_id_by_token(seller_token)
	if err != nil {
		return fmt.Errorf("failed to get seller account ID: %v", err)
	}

	log.Printf("[TRADE] Converted tokens to IDs - Buyer: %s -> %s, Seller: %s -> %s", buyer_token, buyer_id, seller_token, seller_id)

	quote_amount := amount * price
	fee_rate := 0.02 // fee
	buyer_fee := quote_amount * fee_rate
	seller_fee := amount * fee_rate

	net_quote_amount := quote_amount + buyer_fee
	net_base_amount := amount - seller_fee

	log.Printf("[TRADE] Quote amount: %.8f %s, Buyer fee: %.8f %s", quote_amount, quote_asset, buyer_fee, quote_asset)
	log.Printf("[TRADE] Base amount: %.8f %s, Seller fee: %.8f %s", amount, base_asset, seller_fee, base_asset)
	log.Printf("[TRADE] Net amounts - Buyer pays: %.8f %s, Seller receives: %.8f %s", net_quote_amount, quote_asset, net_base_amount, base_asset)

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "REMOVE-ASSETS",
		id:       &buyer_id,
		asset:    &quote_asset,
		amount:   &net_quote_amount,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		log.Printf("[TRADE ERROR] Failed to remove %s from buyer: %v", quote_asset, err)
		log.Println("======")
		return err
	}

	sql_response2 := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "REMOVE-ASSETS",
		id:       &seller_id,
		asset:    &base_asset,
		amount:   &amount,
		response: sql_response2,
	}

	sql_result2 := <-sql_response2
	if err, ok := sql_result2.(error); ok {
		log.Printf("[TRADE ERROR] Failed to remove %s from seller: %v", base_asset, err)
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "ADD-ASSET",
			id:     &buyer_id,
			asset:  &quote_asset,
			amount: &net_quote_amount,
		}
		log.Println("======")
		return err
	}

	sql_response3 := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "ADD-ASSET",
		id:       &buyer_id,
		asset:    &base_asset,
		amount:   &net_base_amount,
		response: sql_response3,
	}

	sql_result3 := <-sql_response3
	if err, ok := sql_result3.(error); ok {
		log.Printf("[TRADE ERROR] Failed to add %s to buyer: %v", base_asset, err)
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "ADD-ASSET",
			id:     &buyer_id,
			asset:  &quote_asset,
			amount: &net_quote_amount,
		}
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "ADD-ASSET",
			id:     &seller_id,
			asset:  &base_asset,
			amount: &amount,
		}
		log.Println("======")
		return err
	}

	sql_response4 := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "ADD-ASSET",
		id:       &seller_id,
		asset:    &quote_asset,
		amount:   &quote_amount,
		response: sql_response4,
	}

	sql_result4 := <-sql_response4
	if err, ok := sql_result4.(error); ok {
		log.Printf("[TRADE ERROR] Failed to add %s to seller: %v", quote_asset, err)
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "ADD-ASSET",
			id:     &buyer_id,
			asset:  &quote_asset,
			amount: &net_quote_amount,
		}
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "ADD-ASSET",
			id:     &seller_id,
			asset:  &base_asset,
			amount: &amount,
		}
		SQL_ORDER_QUEUE <- SQL_QUEUE{
			action: "REMOVE-ASSETS",
			id:     &buyer_id,
			asset:  &base_asset,
			amount: &net_base_amount,
		}
		log.Println("======")
		return err
	}

	log.Printf("[TRADE SUCCESS] Burned fees: %.8f %s + %.8f %s", buyer_fee, quote_asset, seller_fee, base_asset)
	log.Printf("[TRADE SUCCESS] Trade completed successfully")
	log.Println("======")

	return nil
}

func execute_market_order(order *ORDER, estimated_price float64, match_against_type string, user_token string) error {
	ticker := strings.ToUpper(strings.Replace(order.ticker, "-", "/", -1))

	log.Printf("[MARKET ORDER] Executing %s market order for %.8f %s against %s orders",
		order.order_type, order.amount, ticker, match_against_type)

	orderbook, err := get_orderbook(ticker, match_against_type)
	if err != nil {
		return fmt.Errorf("failed to get orderbook: %v", err)
	}

	orders, ok := orderbook.([]map[string]string)
	if !ok {
		return fmt.Errorf("invalid orderbook format")
	}

	if len(orders) == 0 {
		return fmt.Errorf("no liquidity available for %s orders in %s", match_against_type, ticker)
	}

	log.Printf("[MARKET ORDER] Found %d %s orders for matching", len(orders), match_against_type)
	for i, order := range orders {
		price, _ := strconv.ParseFloat(order["price"], 64)
		amount, _ := strconv.ParseFloat(order["amount"], 64)
		log.Printf("[MARKET ORDER] Order %d: %.8f @ %.8f (ID: %s)", i+1, amount, price, order["id"])
	}

	sort.Slice(orders, func(i, j int) bool {
		price_i, _ := strconv.ParseFloat(orders[i]["price"], 64)
		price_j, _ := strconv.ParseFloat(orders[j]["price"], 64)

		if match_against_type == "BUY" {
			return price_i > price_j
		} else {
			return price_i < price_j
		}
	})

	remaining_amount := order.amount
	ticker_parts := strings.Split(ticker, "/")
	base_asset := ticker_parts[0]
	quote_asset := ticker_parts[1]

	log.Printf("[MARKET ORDER] Starting to match %.8f %s against %d %s orders", remaining_amount, base_asset, len(orders), match_against_type)

	for _, limit_order := range orders {
		if remaining_amount <= 0.000001 {
			break
		}

		limit_price, err := strconv.ParseFloat(limit_order["price"], 64)
		if err != nil {
			log.Printf("[MARKET ORDER] Skipping order with invalid price: %v", err)
			continue
		}

		limit_amount, err := strconv.ParseFloat(limit_order["amount"], 64)
		if err != nil {
			log.Printf("[MARKET ORDER] Skipping order with invalid amount: %v", err)
			continue
		}

		limit_id := limit_order["id"]

		limit_token, err := get_token_by_order_id(limit_id)
		if err != nil {
			log.Printf("[MARKET ORDER] Skipping order - failed to get token for order %s: %v", limit_id, err)
			continue
		}

		slippage := 0.10
		if order.order_type == "BUY" && match_against_type == "SELL" {
			if limit_price > estimated_price*(1+slippage) {
				log.Printf("[MARKET ORDER] Price %.8f exceeds slippage tolerance (max: %.8f, estimated: %.8f)", limit_price, estimated_price*(1+slippage), estimated_price)
				break
			}
		} else if order.order_type == "SELL" && match_against_type == "BUY" {
			if limit_price < estimated_price*(1-slippage) {
				log.Printf("[MARKET ORDER] Price %.8f below slippage tolerance (min: %.8f, estimated: %.8f)", limit_price, estimated_price*(1-slippage), estimated_price)
				break
			}
		}

		trade_amount := remaining_amount
		if trade_amount > limit_amount {
			trade_amount = limit_amount
		}

		log.Printf("[MARKET ORDER] Matching %.8f %s at price %.8f with order %s", trade_amount, base_asset, limit_price, limit_id)

		var buyer_token, seller_token string
		if order.order_type == "BUY" {
			buyer_token = user_token
			seller_token = limit_token
		} else {
			buyer_token = limit_token
			seller_token = user_token
		}

		err = execute_trade(buyer_token, seller_token, base_asset, quote_asset, trade_amount, limit_price, limit_id)
		if err != nil {
			log.Printf("[MARKET ORDER ERROR] Failed to execute trade: %v", err)
			return err
		}

		new_limit_amount := limit_amount - trade_amount
		if new_limit_amount > 0.000001 {
			err = update_order_amount(limit_id, new_limit_amount)
			if err != nil {
				log.Printf("[MARKET ORDER ERROR] Failed to update limit order: %v", err)
			}
		} else {
			err = remove_filled_order(limit_id, limit_token)
			if err != nil {
				log.Printf("[MARKET ORDER ERROR] Failed to remove filled order: %v", err)
			}
		}

		remaining_amount -= trade_amount
		log.Printf("[MARKET ORDER] Trade completed. Remaining to fill: %.8f %s", remaining_amount, base_asset)
	}

	if remaining_amount > 0.000001 {
		log.Printf("[MARKET ORDER] Partially filled: %.8f %s remaining unfilled", remaining_amount, base_asset)
	} else {
		log.Printf("[MARKET ORDER] Order completely filled")
	}

	return nil
}

func update_order_amount(order_id string, new_amount float64) error {
	log.Printf("[ORDER UPDATE] Updating order %s to amount %.8f", order_id, new_amount)

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "UPDATE-ORDER-AMOUNT",
		id:       &order_id,
		amount:   &new_amount,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		return fmt.Errorf("failed to update order amount in SQL: %v", err)
	}

	redis_response := make(chan interface{}, 1)
	REDIS_ORDER_QUEUE <- REDIS_QUEUE{
		action:   "UPDATE-ORDER",
		id:       &order_id,
		amount:   &new_amount,
		response: redis_response,
	}

	redis_result := <-redis_response
	if err, ok := redis_result.(error); ok {
		return fmt.Errorf("failed to update order amount in Redis: %v", err)
	}

	log.Printf("[ORDER UPDATE] Updated order %s to amount %.8f", order_id, new_amount)
	return nil
}

func remove_filled_order(order_id string, token string) error {
	success, err := cancel_order(order_id, token)
	if err != nil {
		return fmt.Errorf("failed to remove filled order: %v", err)
	}
	if !success {
		return fmt.Errorf("failed to remove filled order %s", order_id)
	}

	log.Printf("[ORDER REMOVAL] Removed completely filled order %s", order_id)
	return nil
}

func status(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	err = conn.WriteMessage(websocket.TextMessage, []byte("CHECK"))
	if err != nil {
		log.Println("Error during message writing:", err)
	}

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("[ERROR] [WEBSOCKET-CLIENT]", err)
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Println("[INFO] Client closed connection with code:", closeErr.Code)
			}
			break
		}
		handle_client_message(messageType, message)
	}
}

func delete_account_with_token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type token_deletion struct {
		Token string `json:"token"`
	}
	var token_data token_deletion
	body := json.NewDecoder(r.Body)
	err := body.Decode(&token_data)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid form"})
		return
	}
	log.Println("[TOKEN] Token deletion requested for ", token_data.Token)
	if token_data.Token == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Token cannot be empty"})
		return
	}

	token := strings.TrimSpace(token_data.Token)

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "DELETE-USER",
		token:    &token,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Failed to delete account"})
		log.Printf("[ERROR] [DELETE-ACCOUNT] Failed to delete account with token %s: %v", token, err)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Account deleted successfully"})
	log.Printf("[INFO] Account with token %s deleted successfully", token)
}

func create_token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type token_create struct {
		Username string `json:"username"`
	}
	var token_data token_create
	body := json.NewDecoder(r.Body)
	err := body.Decode(&token_data)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid form"})
		return
	}
	log.Println("[TOKEN] Token creation requested for ", token_data.Username)
	username := strings.ToLower(strings.TrimSpace(token_data.Username))
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Username cannot be empty"})
		return
	}

	validUsername := regexp.MustCompile(`^[a-z0-9]+$`).MatchString(username)
	if !validUsername {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Username can only contain small letters and numbers"})
		return
	}

	if len(username) < 3 || len(username) > 30 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Username must be between 3 and 30 characters"})
		return
	}
	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "CREATE-USER",
		username: &username,
		response: sql_response,
	}
	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Error occurred while creating user"})
		log.Printf("[ERROR] [TOKEN] Failed to create user %s: %v", username, err)
	}

	account, ok := sql_result.(ACCOUNT)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Error creating user account. Accountname is maybe already in use."})
		return
	}

	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action: "ADD-ASSET",
		id:     &account.id,
		asset:  &DEFAULT_ASSET_NAME,
		amount: &DEFAULT_ASSET_AMOUNT,
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Account created",
		"account": map[string]string{
			"id":    account.id,
			"name":  account.account_name,
			"token": account.account_token,
		},
	})
	log.Printf("[TOKEN] Created token for user '%s'", username)
}

func get_assets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type token_get struct {
		Id string `json:"id"`
	}
	var tokenData token_get
	body := json.NewDecoder(r.Body)
	err := body.Decode(&tokenData)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid form"})
		return
	}

	if tokenData.Id == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Token cannot be empty"})
		return
	}

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ACCOUNT-ID-BY-TOKEN",
		token:    &tokenData.Id,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		log.Printf("[GET ASSETS] Error getting account ID: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid token or account not found"})
		return
	}

	accountId, ok := sql_result.(string)
	if !ok || accountId == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid token or account not found"})
		return
	}

	sql_response2 := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ALL-ASSETS",
		id:       &accountId,
		response: sql_response2,
	}

	sql_result2 := <-sql_response2
	if err, ok := sql_result2.(error); ok {
		log.Printf("[GET ASSETS] Error fetching assets: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Error while fetching assets"})
		return
	}

	assets, ok := sql_result2.([]map[string]interface{})
	if !ok {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Error while parsing assets"})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Assets fetched",
		"assets":  assets,
	})
}

func ticker_ws(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ticker := vars["ticker"]

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WS upgrade error:", err)
		return
	}
	defer conn.Close()
	log.Println("[WS CONNECTION] Client connected for ticker:", strings.Replace(ticker, "-", "/", -1))

	// FETCH ORDERS FROM REDIS, CONVERT INTO ORDERBOOK, SEND IT TO THE CLIENT

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("[ERROR] [TICKER WEBSOCKET]", err)
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Println("[INFO] Client closed connection with code:", closeErr.Code)
			}
			break
		}
		handle_ticker_actions(messageType, message, conn, ticker)

	}

}

func cancel_order(id string, token string) (bool, error) {
	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "CANCEL-ORDER",
		id:       &id,
		token:    &token,
		response: sql_response,
	}

	sql_result := <-sql_response
	if err, ok := sql_result.(error); ok {
		return false, err
	}

	success, ok := sql_result.(bool)
	if !ok || !success {
		return false, fmt.Errorf("Order #" + id + " could not be deleted")
	}

	return true, nil
}

func cancel_order_handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type Cancel_order struct {
		Id    string `json:"id"`
		Token string `json:"token"`
	}
	var co Cancel_order
	body := json.NewDecoder(r.Body)
	err := body.Decode(&co)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid form"})
		return
	}
	canceled, err := cancel_order(co.Id, co.Token)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Failed to cancel order: " + err.Error()})
		return
	}
	if canceled {
		json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Order #" + co.Id + " canceled successfully"})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Could not cancel order"})
	}
}

func get_all_tickers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// FETCH ALL TICKERS BY GETTING EACH ASSET FROM EACH ACCOUNT

	sql_response := make(chan interface{}, 1)
	SQL_ORDER_QUEUE <- SQL_QUEUE{
		action:   "GET-ALL-TICKERS",
		response: sql_response,
	}
	sql_result := <-sql_response
	tickers, ok := sql_result.([]TICKER_INFO)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Error while fetching tickers."})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Tickers fetched",
		"tickers": tickers,
	})
}

func logging_middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func websocket_handler(w http.ResponseWriter, r *http.Request) {
	// Managing actions as BUY,SELL,TRANSFER,PLACE -> WE HANDLE JSON IN THIS HANDLER
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("[ERROR] [WEBSOCKET-CLIENT]", err)
			if _, ok := err.(*websocket.CloseError); ok {
				log.Println("[INFO] Client closed connection with code:", err)
			}
			break
		}
		var actionData map[string]interface{}
		if err := json.Unmarshal(message, &actionData); err != nil {
			log.Println("[ERROR] Invalid JSON format:", err)
			conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"error","message":"Invalid JSON format"}`))
			continue
		}
		handle_client_action(messageType, actionData, conn)
	}
}

func handle_http() {
	router := mux.NewRouter()
	router.Use(logging_middleware)

	router.HandleFunc("/ws/status", status)
	router.HandleFunc("/ws/ticker/{ticker}", ticker_ws)
	router.HandleFunc("/ws", websocket_handler)
	router.HandleFunc("/api/tickers", get_all_tickers).Methods("GET")
	router.HandleFunc("/api/create", create_token).Methods("POST")
	router.HandleFunc("/api/delete", delete_account_with_token).Methods("POST")
	router.HandleFunc("/api/assets", get_assets).Methods("POST")
	// router.HandleFunc("/api/orders", get_orders).Methods("POST")
	router.HandleFunc("/api/orders/cancel", cancel_order_handler).Methods("POST")

	router.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	staticDir := "./public/"
	if _, err := os.Stat(staticDir); err == nil {
		router.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir(staticDir))))
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3421"
	}

	log.Printf("[HTTP SERVER] Starting server on port %s", port)
	err := http.ListenAndServe(":"+port, router)
	if err != nil {
		log.Println("[ERROR] [HTTP SERVER]: ", err)
	}
}

// ! REDIS

func initialize_redis(ctx context.Context) *redis.Client {
	if err := godotenv.Load(); err != nil {
		log.Println("[WARN] Could not load .env file:", err)
	}
	redisPassword := os.Getenv("REDIS_PASSWORD")

	client := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: redisPassword,
		DB:       0,
	})

	err := client.Set(ctx, "RED", "IS", 0).Err()
	if err != nil {
		panic(err)
	}
	val, err := client.Get(ctx, "RED").Result()
	if err != nil {
		panic(err)
	}
	if val == "IS" {
		log.Println("[REDIS] Connection successful!")
	}
	// RESET DATABASE
	client.FlushDB(ctx)
	return client
}

func initialize_sqlite() *sql.DB {
	db, err := sql.Open("sqlite", "./"+db_file)
	if err != nil {
		panic(err)
	}
	log.Println("[SQLITE3] Database '" + db_file + "' created")
	return db
}

func create_sqlite_tables(sql_driver *sql.DB) {
	// CREATING ACCOUNTS
	_, err := sql_driver.Exec(SQL_accounts_table)
	if err != nil {
		panic(err)
	}
	// CREATING ASSETS
	_, erra := sql_driver.Exec(SQL_assets_table)
	if erra != nil {
		panic(erra)
	}
	// CREATING ORDERS
	_, erro := sql_driver.Exec(SQL_orders_table)
	if erro != nil {
		panic(erro)
	}

	// CREATE INDEXES FOR PERFORMANCE
	_, errIdx1 := sql_driver.Exec("CREATE INDEX IF NOT EXISTS idx_accounts_name ON accounts(account_name)")
	if errIdx1 != nil {
		log.Printf("[WARNING] Failed to create index on accounts.account_name: %v", errIdx1)
	}

	_, errIdx2 := sql_driver.Exec("CREATE INDEX IF NOT EXISTS idx_accounts_token ON accounts(account_token)")
	if errIdx2 != nil {
		log.Printf("[WARNING] Failed to create index on accounts.account_token: %v", errIdx2)
	}

	_, errIdx3 := sql_driver.Exec("CREATE INDEX IF NOT EXISTS idx_assets_account_id ON assets(account_id)")
	if errIdx3 != nil {
		log.Printf("[WARNING] Failed to create index on assets.account_id: %v", errIdx3)
	}
	_, errIdx4 := sql_driver.Exec("CREATE INDEX IF NOT EXISTS idx_orders_token ON orders(token)")
	if errIdx4 != nil {
		log.Printf("[WARNING] Failed to create index on orders.token: %v", errIdx4)
	}

	log.Println("[+] CREATED TABLES: accounts, assets, orders")
	log.Println("[+] CREATED INDEXES: account_name, account_token, assets(account_id), orders(token)")
}

func create_redis_order(ctx context.Context, redis_client *redis.Client, id string, order_type string, amount float64, ticker string, price float64, token string) (*ORDER, error) {
	pipe := redis_client.Pipeline()
	pipe.HSet(ctx, "order:"+id, "id", id)
	pipe.HSet(ctx, "order:"+id, "order_type", order_type)
	pipe.HSet(ctx, "order:"+id, "amount", fmt.Sprintf("%f", amount))
	pipe.HSet(ctx, "order:"+id, "ticker", ticker)
	pipe.HSet(ctx, "order:"+id, "price", fmt.Sprintf("%f", price))
	pipe.HSet(ctx, "order:"+id, "token", token)
	_, err := pipe.Exec(ctx)

	if err != nil {
		log.Println("[REDIS ERROR]", err)
		return nil, err
	}

	var bookKey string
	if order_type == "BUY" {
		bookKey = "orderbook:" + ticker + ":buy"
		if err = redis_client.ZAdd(ctx, bookKey, redis.Z{
			Score:  -price,
			Member: id,
		}).Err(); err != nil {
			return nil, err
		}
	} else {
		bookKey = "orderbook:" + ticker + ":sell"
		if err = redis_client.ZAdd(ctx, bookKey, redis.Z{
			Score:  price,
			Member: id,
		}).Err(); err != nil {
			return nil, err
		}
	}

	return &ORDER{
		id:         id,
		order_type: order_type,
		amount:     amount,
		ticker:     ticker,
		price:      price,
		token:      token,
	}, nil
}

func delete_redis_order(ctx context.Context, redis_client *redis.Client, id string) {
	orderData, err := redis_client.HGetAll(ctx, "order:"+id).Result()
	if err != nil {
		log.Printf("[ERROR] [REDIS] Failed to get order data for %s: %v", id, err)
		return
	}

	if len(orderData) > 0 {
		ticker := orderData["ticker"]
		orderType := strings.ToLower(orderData["order_type"])

		bookKey := fmt.Sprintf("orderbook:%s:%s", ticker, orderType)
		redis_client.ZRem(ctx, bookKey, id)
	}

	err = redis_client.Del(ctx, "order:"+id).Err()
	if err != nil {
		log.Printf("[ERROR] [REDIS] Failed to delete order %s: %v", id, err)
	}
}

// ~ SQL

func get_all_orders_from_sql(sql_driver *sql.DB) (*sql.Rows, error) {
	rows, err := sql_driver.Query("SELECT * FROM orders")

	if err != nil {
		log.Println("[ERROR] [SQLITE]", err)
		return nil, err
	}

	return rows, nil
}

func insert_order_into_sql(sql_driver *sql.DB, order_type string, amount float64, ticker string, price float64, token string, idOpt ...string) (*ORDER, error) {
	var id string
	if len(idOpt) > 0 && idOpt[0] != "" {
		id = idOpt[0]
	} else {
		id = uuid.NewString()
	}

	insert := `INSERT INTO orders(id,order_type,amount,ticker,price,token)
	VALUES (?,?,?,?,?,?)`

	_, err := sql_driver.Exec(insert, id, order_type, amount, ticker, price, token)
	if err != nil {
		return nil, err
	}

	return &ORDER{
		id:         id,
		order_type: order_type,
		amount:     amount,
		ticker:     ticker,
		price:      price,
		token:      token,
	}, nil
}

func delete_order_from_sql(sql_driver *sql.DB, order_id string) error {
	insert := `DELETE FROM orders WHERE id == ?`

	_, err := sql_driver.Exec(insert, order_id)
	if err != nil {
		return err
	}
	return nil
}

func EXAMPLE_ORDERS(sql_driver *sql.DB, redis_client *redis.Client) {
	exampleOrders := []struct {
		orderType string
		amount    float64
		price     float64
		token     string
	}{
		{"BUY", 10.0, 5.25, "user1_token"},
		{"BUY", 5.5, 5.20, "user2_token"},
		{"BUY", 8.0, 5.15, "user3_token"},
		{"BUY", 12.5, 5.10, "user4_token"},
		{"BUY", 15.0, 5.05, "user5_token"},
		{"SELL", 7.0, 5.30, "user6_token"},
		{"SELL", 9.5, 5.35, "user7_token"},
		{"SELL", 6.25, 5.40, "user8_token"},
		{"SELL", 11.0, 5.45, "user9_token"},
		{"SELL", 20.0, 5.50, "user10_token"},
	}

	log.Println("[~] Creating example orders for SIGE/HACK")
	for i, o := range exampleOrders {
		id := uuid.NewString()
		_, err := insert_order_into_sql(sql_driver, o.orderType, o.amount, "SIGE/HACK", o.price, o.token, id)
		if err != nil {
			log.Printf("[ERROR] Failed to create example order %d: %v", i+1, err)
			continue
		}

	}
	log.Println("[+] Example orders created successfully")
}

func main() {
	log.Println("[~] Starting SIEGE CT SERVER...")
	// ! INITIALIZE REDIS SERVICE
	var redis_client *redis.Client = initialize_redis(ctx)
	// ! INITIALIZE SQLITE SERVICE
	var sql_driver *sql.DB = initialize_sqlite()
	defer sql_driver.Close()
	// ! CREATING SQL TABLES IF NOT EXISTING, accounts, their assets and orders
	create_sqlite_tables(sql_driver)
	// ! GET ALL ORDERS FROM SQLITE INTO REDIS
	// EXAMPLE_ORDERS(sql_driver, redis_client)
	orders, errO := get_all_orders_from_sql(sql_driver)
	if errO != nil {
		log.Println("[ERROR] [SQL] Failed to get orders:", errO)
	}
	log.Println("[~] Fetching saved orders:")
	//! DEBUG
	// var idsToDelete []string
	// ! ------ ------- - -- - - - ---
	for orders.Next() {
		var order ORDER
		errOF := orders.Scan(&order.id, &order.order_type, &order.amount, &order.ticker, &order.price, &order.token)
		if errOF != nil {
			log.Println("[ERROR] [SQL] Failed to scan order:", errOF)
			continue
		}
		//! idsToDelete = append(idsToDelete, order.id)
		create_redis_order(ctx, redis_client, order.id, order.order_type, order.amount, strings.ToUpper(order.ticker), order.price, order.token)
		log.Println("	-+ " + order.id)
	}

	//! DEBUG
	// for _, id := range idsToDelete {
	// delete_order_from_sql(sql_driver, id)
	// }
	//!-------- - - - - - - - --- - -

	// ! START THE QUEUE FOR ORDERS

	go redis_queue_worker(sql_driver, REDIS_ORDER_QUEUE, ctx, redis_client)
	go sql_queue_worker(sql_driver, SQL_ORDER_QUEUE)
	go order_fulfiller_worker(ORDER_FULFILL_QUEUE)

	// ! START THE LIMIT ORDER MANAGER
	go limit_order_manager(sql_driver, ctx, redis_client)

	//! DEBUG
	// accountId := "260b2858-4b2b-44a1-86d2-0a54ae18e907"
	// assetName := "HACK"
	// SQL_ORDER_QUEUE <- SQL_QUEUE{
	// 	action: "ADD-ASSET",
	// 	id:     &accountId,
	// 	asset:  &assetName,
	// 	amount: &DEFAULT_ASSET_AMOUNT,
	// }
	//!-------- - - - - - - - --- - -

	fmt.Println("----------------------\nSIEGE CT SERVER IS READY\n----------------------")
	handle_http()

}
