package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"log"
	"net/http"
	"strconv"
	"time"
)

var mongoClient *mongo.Client
var dbName = "aspireDB"
var chatsCollection = "chats"

func main() {
	// Initialize MongoDB connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var err error
	mongoClient, err = mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		if err := mongoClient.Disconnect(ctx); err != nil {
			log.Fatal(err)
		}
	}()

	createIndex()

	go func() {
		http.HandleFunc("/chat", handleConnections)

		fmt.Println("Starting websocket server on port 11112...")
		log.Fatal(http.ListenAndServe(":11112", nil))
	}()

	http.HandleFunc("/messages", handleFetchMessages)
	log.Println("Starting http server on port 11113...")
	log.Fatal(http.ListenAndServe(":11113", nil))
}

// Client represents a single chat user
type Client struct {
	ID   string
	Conn *websocket.Conn
	Send chan []byte
}

// Message represents a chat message
type Message struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Content string `json:"content"`
	Time    int64  `json:"time"`
}

// ChatMessage represents the document form of a chat message for MongoDB
type ChatMessage struct {
	ChatID  string `json:"chatID"`
	From    string `json:"from"`
	To      string `json:"to"`
	Content string `json:"content"`
	Time    int64  `json:"time"`
}

var clients = make(map[string]*Client)     // Track connected clients
var chatRooms = make(map[string][]*Client) // Map chatRooms to clients

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow any origin
	},
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Error upgrading... " + err.Error())
		log.Println(err)
	}

	studentID := r.URL.Query().Get("studentID")
	client := &Client{ID: studentID, Conn: conn, Send: make(chan []byte)}

	log.Print("Client connected: ")
	log.Println(conn.RemoteAddr())

	clients[studentID] = client
	go handleMessages(client)
	go client.writeMessages()
}

func handleMessages(client *Client) {
	defer client.Conn.Close()

	for {
		var msg Message
		err := client.Conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("error: %v", err)
			delete(clients, client.ID)
			break
		}
		log.Print("Reading message: ")
		fmt.Printf("From: %s", msg.From)
		fmt.Println()
		fmt.Printf("To: %s", msg.To)
		fmt.Println()
		fmt.Printf("Content: %s", msg.Content)
		fmt.Println()
		fmt.Printf("Time: %d", msg.Time)
		fmt.Println()

		chatRoomID := getChatRoomID(msg.From, msg.To)
		fmt.Printf("Chat Room ID: %s", chatRoomID)

		if _, ok := chatRooms[chatRoomID]; !ok {
			chatRooms[chatRoomID] = []*Client{}
		}

		for _, chatClient := range chatRooms[chatRoomID] {
			if chatClient.ID == msg.To {
				chatClient.Send <- []byte(fmt.Sprintf("%s: %s,\n%d", msg.From, msg.Content, msg.Time))
			}
		}
	}
}

func getChatRoomID(userA, userB string) string {
	if userA < userB {
		return userA + "_" + userB
	}
	return userB + "_" + userA
}

func (c *Client) writeMessages() {
	defer c.Conn.Close()

	for msg := range c.Send {
		err := c.Conn.WriteMessage(websocket.TextMessage, msg)
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
	}
}

func saveMessageToDB(msg ChatMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(chatsCollection)

	_, err := collection.InsertOne(ctx, bson.M{
		"chatID":  msg.ChatID,
		"from":    msg.From,
		"to":      msg.To,
		"content": msg.Content,
		"time":    msg.Time,
	})

	if err != nil {
		log.Println("Error writing to MongoDB:", err)
	} else {
		log.Println("Message saved to MongoDB:", msg)
	}
}

func handleFetchMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	chatID := r.URL.Query().Get("chatID")
	pageStr := r.URL.Query().Get("page")
	limitStr := r.URL.Query().Get("limit")

	fmt.Println("Fetching messages...")
	fmt.Printf("ChatID: %s", chatID)
	fmt.Println()
	fmt.Printf("Page: %s", pageStr)
	fmt.Println()
	fmt.Printf("Limit: %s", limitStr)
	fmt.Println()

	if chatID == "" {
		http.Error(w, "chatID is required", http.StatusBadRequest)
		return
	}

	page := 1
	limit := 50
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil {
			page = p
		}
	}
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}

	messages, err := fetchMessages(chatID, page, limit)
	if err != nil {
		http.Error(w, "Failed to fetch messages: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Respond with messages in JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

func fetchMessages(chatID string, page, limit int) ([]bson.M, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(chatsCollection)

	skip := (page - 1) * limit
	opts := options.Find().SetSort(bson.D{{"time", 1}}).SetSkip(int64(skip)).SetLimit(int64(limit))
	filter := bson.M{"chatID": chatID}
	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var messages []bson.M
	if err = cursor.All(ctx, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}

func createIndex() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(chatsCollection)

	indexModel := mongo.IndexModel{
		Keys: bson.M{"chatID": -1}, // 1 for ascending order
	}

	_, err := collection.Indexes().CreateOne(ctx, indexModel)
	if err != nil {
		log.Fatal("Error creating index:", err)
	}
	log.Println("Index created on chatID field")
}
