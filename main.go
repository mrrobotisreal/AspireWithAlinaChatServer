package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
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
var studentsCollection = "students"
var teachersCollection = "teachers"

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

	certFile := "/etc/letsencrypt/live/aspirewithalina.com/fullchain.pem"
	keyFile := "/etc/letsencrypt/live/aspirewithalina.com/privkey.pem"

	go func() {
		http.HandleFunc("/chat", handleConnections)

		fmt.Println("Starting websocket server on port 11112...")
		if err := http.ListenAndServeTLS(":11112", certFile, keyFile, nil); err != nil {
			log.Fatalf("Failed to start websocket server on port 11112: %v", err)
		}
		//log.Fatal(http.ListenAndServe(":11112", nil))
	}()

	http.HandleFunc("/chats", handleFetchRecentChats)
	http.HandleFunc("/messages", handleFetchMessages)
	http.HandleFunc("/messages/delete", handleDeleteMessages)
	http.HandleFunc("/message", handleMessage)
	log.Println("Starting http server on port 11113...")
	//log.Fatal(http.ListenAndServe(":11113", nil))
	if err := http.ListenAndServeTLS(":11113", certFile, keyFile, nil); err != nil {
		log.Fatalf("Failed to start http server on port 11113: %v", err)
	}
}

// Client represents a single chat user
type Client struct {
	ID   string
	Conn *websocket.Conn
	Send chan []byte
}

// Message represents a chat message
type Message struct {
	From          string `json:"from"`
	FromID        string `json:"fromID"`
	To            string `json:"to"`
	ToID          string `json:"toID"`
	Content       string `json:"content"`
	Time          int64  `json:"time"`
	IsToTeacher   bool   `json:"is_to_teacher"`
	IsFromTeacher bool   `json:"is_from_teacher"`
}

// ChatMessage represents the document form of a chat message for MongoDB
type ChatMessage struct {
	ChatID        string `json:"chatID"`
	From          string `json:"from"`
	FromID        string `json:"fromID"`
	To            string `json:"to"`
	ToID          string `json:"toID"`
	Content       string `json:"content"`
	Time          int64  `json:"time"`
	IsToTeacher   bool   `json:"is_to_teacher"`
	IsFromTeacher bool   `json:"is_from_teacher"`
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
	//log.Println(conn.RemoteAddr())

	clients[studentID] = client

	chatRoomID := "global"
	if _, ok := chatRooms[chatRoomID]; !ok {
		chatRooms[chatRoomID] = []*Client{}
	}
	chatRooms[chatRoomID] = append(chatRooms[chatRoomID], client)

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
		fmt.Printf("From: %s (", msg.From)
		fmt.Printf("%s)", msg.FromID)
		fmt.Println()
		fmt.Printf("To: %s (", msg.To)
		fmt.Printf("%s)", msg.ToID)
		fmt.Println()
		fmt.Printf("Content: %s", msg.Content)
		fmt.Println()
		fmt.Printf("Time: %d", msg.Time)
		fmt.Println()
		fmt.Println("IsToTeacher:", msg.IsToTeacher)
		fmt.Println("IsFromTeacher:", msg.IsFromTeacher)

		chatRoomID := getChatRoomID(msg.FromID, msg.ToID)
		fmt.Printf("Chat Room ID: %s", chatRoomID)
		fmt.Println()

		if _, ok := chatRooms[chatRoomID]; !ok {
			fmt.Println("!ok, adding chat room")
			chatRooms[chatRoomID] = []*Client{
				client,
			}

			// TODO: remove the conditionals below; for testing only
			if client.ID != msg.FromID {
				recipientClient := &Client{
					ID:   msg.FromID,
					Conn: client.Conn,
					Send: make(chan []byte),
				}
				chatRooms[chatRoomID] = append(chatRooms[chatRoomID], recipientClient)
			} else if client.ID != msg.ToID {
				recipientClient := &Client{
					ID:   msg.ToID,
					Conn: client.Conn,
					Send: make(chan []byte),
				}
				chatRooms[chatRoomID] = append(chatRooms[chatRoomID], recipientClient)
			}
		}

		chatMessage := ChatMessage{
			ChatID:        chatRoomID,
			From:          msg.From,
			FromID:        msg.FromID,
			To:            msg.To,
			ToID:          msg.ToID,
			Content:       msg.Content,
			Time:          msg.Time,
			IsToTeacher:   msg.IsToTeacher,
			IsFromTeacher: msg.IsFromTeacher,
		}

		for _, chatClient := range chatRooms[chatRoomID] {
			fmt.Println("chatClientID: " + chatClient.ID)
			if chatClient.ID == msg.To {
				messageJSON, err := json.Marshal(chatMessage)
				if err != nil {
					log.Printf("Error serializing message: %v", err)
					continue // Skip sending this message
				}

				chatClient.Send <- messageJSON
			}
		}

		// Save to chatHistory database no matter what,
		// in case any WS issues, then at least http can
		// fetch the messages, and they're still accessible.
		saveMessageToDB(chatMessage)
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
		"chatID":        msg.ChatID,
		"messageID":     uuid.New(),
		"from":          msg.From,
		"fromID":        msg.FromID,
		"to":            msg.To,
		"toID":          msg.ToID,
		"content":       msg.Content,
		"time":          msg.Time,
		"istoteacher":   msg.IsToTeacher,
		"isfromteacher": msg.IsFromTeacher,
	})

	if err != nil {
		log.Println("Error writing to MongoDB:", err)
	} else {
		log.Println("Message saved to MongoDB:", msg.ChatID)
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

func handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req Message
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			fmt.Println("Invalid request body during POST request...")
			return
		}
		postMessage(req)
	}
}

func postMessage(msg Message) error {
	chatRoomID := getChatRoomID(msg.ToID, msg.FromID)
	newChatMsg := ChatMessage{
		ChatID:        chatRoomID,
		From:          msg.From,
		FromID:        msg.FromID,
		Time:          msg.Time,
		To:            msg.To,
		ToID:          msg.ToID,
		IsToTeacher:   msg.IsToTeacher,
		IsFromTeacher: msg.IsFromTeacher,
	}
	saveMessageToDB(newChatMsg)
	return nil
}

func putMessage() {}

func deleteMessage() {}

type DeleteAllChatMessagesRequest struct {
	ChatID string `json:"chat_id"`
}

type DeleteAllChatMessagesResponse struct {
	IsDeleted bool `json:"is_deleted"`
}

func handleDeleteMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	var req DeleteAllChatMessagesRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	response, err := deleteMessages(req.ChatID)
	if err != nil {
		http.Error(w, "Error deleting all messages for chatID: "+req.ChatID, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func deleteMessages(chatID string) (DeleteAllChatMessagesResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(chatsCollection)
	_, err := collection.DeleteMany(ctx, bson.M{"chatid": chatID})
	if err != nil {
		fmt.Println("Error finding and deleting all chat messages for chatID:", chatID)
		fmt.Println("Error message:", err)
		return DeleteAllChatMessagesResponse{
			IsDeleted: false,
		}, err
	}

	return DeleteAllChatMessagesResponse{
		IsDeleted: true,
	}, nil
}

func handleFetchRecentChats(w http.ResponseWriter, r *http.Request) {
	var ID string
	var isTeacher bool
	studentID := r.URL.Query().Get("studentID")
	teacherID := r.URL.Query().Get("teacherID")
	if studentID == "" && teacherID == "" {
		http.Error(w, "studentID or teacherID is required", http.StatusBadRequest)
		return
	}
	if studentID == "" && teacherID != "" {
		ID = teacherID
		isTeacher = true
	}
	if teacherID == "" && studentID != "" {
		ID = studentID
		isTeacher = false
	}

	// Fetch recent chats
	chats, err := fetchRecentChats(ID, isTeacher)
	if err != nil {
		http.Error(w, "Failed to fetch recent chats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Respond with JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
}

func fetchRecentChats(ID string, isTeacher bool) ([]bson.M, error) {
	var userID string
	var studentInfo Student
	var teacherInfo Teacher
	if isTeacher {
		// TODO: maybe remove these? I don't remember why I even needed it in the first place. I can just use ID
		teacherInfo, _ = getTeacherInfo(ID)
		userID = teacherInfo.TeacherID
		studentInfo = Student{}
	} else {
		// TODO: maybe remove these? I don't remember why I even needed it in the first place. I can just use ID
		studentInfo, _ = getStudentInfo(ID)
		userID = studentInfo.StudentId
		teacherInfo = Teacher{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(chatsCollection)

	//matchStage := bson.D{{"$match", bson.D{{}}}}
	pipeline := mongo.Pipeline{
		// Match messages involving the user
		{{"$match", bson.M{
			"$or": []bson.M{
				{"fromID": userID},
				{"toID": userID},
			},
		}}},
		// Sort messages by time descending
		{{"$sort", bson.D{{"time", -1}}}},
		// Group by chatID
		{{"$group", bson.M{
			"_id":               "$chatID",
			"mostRecentMessage": bson.M{"$first": "$$ROOT"}, // First message (most recent)
		}}},
		// Add 'chatID' and 'to' to the final output
		{{"$project", bson.M{
			"chatID":            "$_id",
			"to":                "$mostRecentMessage.to",   // TODO: Remove, not needed. Update Electron app first though
			"toID":              "$mostRecentMessage.toID", // TODO: Remove, not needed. Update Electron app first though
			"mostRecentMessage": "$mostRecentMessage",
			"_id":               0, // Exclude MongoDB internal _id
		}}},
	}

	cursor, err := collection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var results []bson.M
	if err = cursor.All(ctx, &results); err != nil {
		return nil, err
	}

	return results, nil
}

type Teacher struct {
	TeacherID          string `json:"teacherID"`
	FirstName          string `json:"first_name"`
	PreferredName      string `json:"preferred_name"`
	LastName           string `json:"last_name"`
	NativeLanguage     string `json:"native_language"`
	PreferredLanguage  string `json:"preferred_language"`
	EmailAddress       string `json:"email_address"`
	Password           string `json:"password"`
	Salt               string `json:"salt"`
	ProfilePictureURL  string `json:"profile_picture_url"`
	ProfilePicturePath string `json:"profile_picture_path"`
	ThemeMode          string `json:"theme_mode"`
	FontStyle          string `json:"font_style"`
	TimeZone           string `json:"time_zone"`
	LessonsTaught      int64  `json:"lessons_taught"`
}

type Student struct {
	StudentId          string `json:"student_id"`
	FirstName          string `json:"first_name"`
	PreferredName      string `json:"preferred_name"`
	LastName           string `json:"last_name"`
	EmailAddress       string `json:"email_address"`
	Password           string `json:"password"`
	Salt               string `json:"salt"`
	NativeLanguage     string `json:"native_language"`
	PreferredLanguage  string `json:"preferred_language"`
	StudentSince       string `json:"student_since"`
	ProfilePicturePath string `json:"profile_picture_path"`
	ThemeMode          string `json:"theme_mode"`
	FontStyle          string `json:"font_style"`
	TimeZone           string `json:"time_zone"`
	LessonsRemaining   int32  `json:"lessons_remaining"`
	LessonsCompleted   int32  `json:"lessons_completed"`
}

func getTeacherInfo(teacherID string) (Teacher, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(teachersCollection)

	var teacherInfo Teacher
	err := collection.FindOne(ctx, bson.M{"teacherID": teacherID}).Decode(&teacherInfo)
	if err != nil {
		log.Println("Error getting teacherInfo: " + err.Error())
		return Teacher{}, err
	}

	return teacherInfo, nil
}

func getStudentInfo(studentID string) (Student, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := mongoClient.Database(dbName).Collection(studentsCollection)

	var studentInfo Student
	err := collection.FindOne(ctx, bson.M{"studentid": studentID}).Decode(&studentInfo)
	if err != nil {
		log.Println("Error getting studentInfo: " + err.Error())
		return Student{}, err
	}

	return studentInfo, nil
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
