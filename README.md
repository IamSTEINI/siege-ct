# 🏛️ SIEGE CT (Siege Coin Trader)

Welcome to SIEGE CT, a trading simulation that demonstrates how cryptocurrency exchanges and their trading clients work behind the scenes. This isn't a real cryptocurrency or exchange, but rather an educational simulation that shows you exactly what happens when you place orders, execute trades, and interact with orderbooks.

> 💡 **Think of it like this:** Ever wondered what happens when you click "Buy" on a crypto exchange? SIEGE CT lets you peek behind the curtain and see the actual mechanics in action.

## 🎯 What is SIEGE CT?

**DEMO:** https://youtu.be/IvVmYbKACPE
SIEGE CT simulates a complete trading ecosystem where you can experience real exchange mechanics without any financial risk. You get to see how market orders work, how limit orders execute when prices are reached, and how the overall market price gets determined through actual trading activity.

The system consists of two main parts: a server that handles all the trading logic and a terminal client that lets you interact with the market. Both work together to create a realistic trading experience.

```
📊 Real Orderbooks  →  💰 Actual Trading  →  📈 Price Discovery
```

## ⚡ How Trading Works Here

When you place orders in SIEGE CT, you're participating in a real orderbook system. The server maintains buy and sell orders for different trading pairs, and when conditions are right, orders automatically execute against each other.

### 🚀 Market Orders
Market orders execute immediately at the current market price. When you place a market order, the system looks at existing limit orders and fills your order using the best available prices. This means you're actually trading with other users who have placed limit orders.

**Example:** You want to buy 10 HACK tokens right now? Boom! Your market order grabs the best available sell orders instantly (if you have the assets to).

### ⏰ Limit Orders  
Limit orders sit in the orderbook waiting for the right price. If you place a buy limit order at 5.00, it will only execute when the market price drops to 5.00 or below. The server continuously monitors all limit orders and automatically converts them to market orders when the price conditions are met.

**Example:** Set a buy order for MOON tokens at 2.50 SIGE each, then go grab coffee. When the price hits your target, the system automatically executes your trade.

### 📊 Price Discovery
The market price gets calculated using VWAP (Volume Weighted Average Price) from the current orderbook. This means the price reflects actual trading interest and volume, just like in real exchanges. As more orders get placed and executed, the price naturally moves based on supply and demand.

> 🎪 **Fun fact:** Every trade you make actually affects the market price for everyone else!

## 🖥️ The Server

The server is the heart of SIEGE CT. It handles everything that happens behind the scenes:

🗄️ **Order management** and storage in both Redis and SQLite databases  
📋 **Real-time orderbook** maintenance for all trading pairs  
🎯 **Automatic execution** of limit orders when price targets are hit  
💹 **Market price calculation** using orderbook data  
👤 **User account** and asset management  
🔌 **WebSocket connections** for real-time trading  

The server runs continuously, monitoring all active limit orders every few seconds. When it detects that a limit order should execute (because the market price has reached the limit price), it automatically converts that order into a market order and processes it immediately.

**Like your personal trading robot that never sleeps!** 🤖

## 💻 The Terminal

The terminal is your trading interface. Through it, you can:

📊 **View live orderbooks** with current buy and sell orders  
⚡ **Place market orders** that execute instantly  
⏱️ **Set limit orders** that wait for your target price  
💰 **Check your account** balance and assets  
🎯 **See all available** trading pairs  

The terminal connects directly to the server and gives you real-time access to the trading system. Everything you do gets processed immediately, and you can see the results of your trades right away.

```
Terminal  →  Server  →  Database  →  Instant Results! 
```

## 🪙 Trading Pairs and Assets

SIEGE CT supports multiple trading pairs. Every pair follows the format BASE/QUOTE (like HACK/SIGE or MOON/SIGE). When you trade, you're exchanging the base asset for the quote asset at the agreed price.

**Want your own ticker?** 🎪 Send me a message on Slack through HackClub! Custom tickers follow the 4LETTERS/SIGE format, so think of something creative. Maybe DOGE/SIGE? COOL/SIGE? The choice is yours! Every ticker gets combined with every other!

## 🌐 Web Version Coming Soon

I'm currently working on a web interface for SIEGE CT that will launch during the next Siege week. The web version will include all the terminal functionality plus additional features like viewing your order history and a more user-friendly trading interface.

The web version will also include the ability to view all your past and current orders, making it easier to track your trading activity over time.

> 🚧 **Coming Soon:** Beautiful charts, order history, portfolio tracking, and much more!

## 🚀 Getting Started

To start trading, you'll need to run the terminal. The server is hosted by myself but if you want to, you can experience your own private server.

Your account starts with some SIGE tokens, which serve as the base currency for most trading pairs. From there, you can trade into other assets and build your portfolio.

**Pro tip:** Start small, learn the mechanics, then go wild!! 📈

## 🔧 Technical Notes

The system uses Go for the server backend with Redis for real-time data and SQLite for persistent storage. The terminal client also runs in Go and communicates with the server through WebSocket connections for real-time updates.

All trading logic follows real exchange principles, including proper asset validation, order matching, and price discovery mechanisms. This makes SIEGE CT an excellent way to understand how actual exchanges work under the hood (a bit :) ).


