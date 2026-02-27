package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- STRUKTUR DATA ---

type Msg struct {
	ID         interface{} `json:"id"`
	ChatJID    string      `json:"chat_jid"`
	SenderName string      `json:"sender_name"`
	Content    string      `json:"content"`
	IsMe       bool        `json:"is_me"`
	Status     string      `json:"status"`
	Timestamp  string      `json:"timestamp"`
	RawTime    string      `json:"raw_time"`
}

// --- GLOBAL VARS ---
var (
	waClient  *whatsmeow.Client
	dbConn    *sql.DB
	irisDB    *sql.DB
	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex
	myJID     string
)

func init() {
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️  Warning: No .env file found")
	}
}

func main() {
	initSQLite()
	initIRIS()
	initWhatsApp()

	app := fiber.New(fiber.Config{
		BodyLimit: 50 * 1024 * 1024,
		AppName:   "HPII WhatsApp Gateway v1.2",
	})

	app.Static("/", "./templates")

	// API Routes
	api := app.Group("/api")
	api.Get("/contacts", getContacts)
	api.Get("/messages/:jid", getMessages)
	api.Post("/send", sendMessage)
	api.Post("/mark-read", markMessageAsRead) // Update status otomatis
	api.Get("/search", searchMessagesGET)
	api.Post("/search", searchMessagesPOST)
	api.Post("/logout", logoutWA)

	api.Get("/iris-test", func(c *fiber.Ctx) error {
		if irisDB == nil {
			return c.Status(500).SendString("Koneksi IRIS tidak aktif")
		}
		var sysDate string
		err := irisDB.QueryRow("SELECT CURRENT_DATE").Scan(&sysDate)
		if err != nil {
			return c.Status(500).SendString(err.Error())
		}
		return c.JSON(fiber.Map{"iris_date": sysDate})
	})

	app.Get("/ws", websocket.New(wsHandler))

	go func() {
		fmt.Println("🔌 Memulai koneksi WhatsApp...")
		connectWA()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		fmt.Println("\n🛑 Mematikan server...")
		if waClient != nil {
			waClient.Disconnect()
		}
		if dbConn != nil {
			dbConn.Close()
		}
		if irisDB != nil {
			irisDB.Close()
		}
		app.Shutdown()
		os.Exit(0)
	}()

	port := os.Getenv("APP_PORT")
	if port == "" {
		port = "5007"
	}

	fmt.Printf("🚀 Dashboard Aktif: http://localhost:%s\n", port)
	log.Fatal(app.Listen(":" + port))
}

// --- DATABASE LAYER ---

func initSQLite() {
	var err error
	dbConn, err = sql.Open("sqlite", "file:wa_asli.db?_foreign_keys=on&_journal_mode=WAL&cache=shared")
	if err != nil {
		log.Fatal("❌ Gagal membuka SQLite:", err)
	}

	schemas := []string{
		`CREATE TABLE IF NOT EXISTS group_chat_logs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            chat_id TEXT,
            sender_name TEXT,
            message TEXT,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );`,
		`CREATE TABLE IF NOT EXISTS messages (
            id TEXT PRIMARY KEY,
            chat_jid TEXT,
            sender_jid TEXT,
            sender_name TEXT,
            content TEXT,
            is_from_me BOOLEAN,
            timestamp DATETIME,
            status TEXT DEFAULT 'sent'
        );`,
		`CREATE INDEX IF NOT EXISTS idx_group_chat ON group_chat_logs(chat_id);`,
		`CREATE INDEX IF NOT EXISTS idx_msg_chat ON messages(chat_jid);`,
	}

	for _, s := range schemas {
		if _, err := dbConn.Exec(s); err != nil {
			log.Fatal("❌ Error Schema SQLite:", err)
		}
	}
	fmt.Println("✅ SQLite Siap")
}

func initIRIS() {
	dsn := os.Getenv("IRIS_DSN")
	if dsn == "" {
		return
	}
	connString := fmt.Sprintf("DSN=%s;Uid=%s;Pwd=%s", dsn, os.Getenv("IRIS_USER"), os.Getenv("IRIS_PASSWORD"))
	var err error
	irisDB, err = sql.Open("odbc", connString)
	if err != nil {
		log.Printf("❌ Gagal ODBC IRIS: %v\n", err)
		return
	}
	if err := irisDB.Ping(); err != nil {
		log.Printf("❌ IRIS Ping Gagal: %v\n", err)
	} else {
		fmt.Println("✅ InterSystems IRIS Terhubung")
	}
}

// --- WHATSAPP LOGIC ---

func initWhatsApp() {
	dbLog := waLog.Stdout("Database", "ERROR", true)
	container := sqlstore.NewWithDB(dbConn, "sqlite", dbLog)
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatal("❌ Gagal ambil DeviceStore:", err)
	}
	waClient = whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", "INFO", true))
	waClient.AddEventHandler(eventHandler)

	if waClient.Store.ID != nil {
		myJID = waClient.Store.ID.ToNonAD().String()
	}
}

func connectWA() {
	if waClient.Store.ID == nil {
		qrChan, _ := waClient.GetQRChannel(context.Background())
		if err := waClient.Connect(); err != nil {
			log.Fatal(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		if err := waClient.Connect(); err != nil {
			log.Fatal(err)
		}
	}
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		content := getText(v.Message)
		if content != "" {
			name := v.Info.PushName
			if name == "" {
				name = v.Info.Sender.User
			}
			saveAndBroadcast(v.Info.Chat.String(), name, content, v.Info.IsFromMe, "delivered", v.Info.Timestamp, v.Info.ID)
		}

	case *events.Receipt:
		for _, msgID := range v.MessageIDs {
			status := "sent"
			switch v.Type {
			case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
				status = "read"
			case types.ReceiptTypeDelivered:
				status = "delivered"
			}
			dbConn.Exec("UPDATE messages SET status=? WHERE id=?", status, msgID)
		}
	}
}

// --- API HANDLERS ---

func markMessageAsRead(c *fiber.Ctx) error {
	var req struct {
		ID  string `json:"id"`
		JID string `json:"jid"`
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// 1. Update Database Lokal
	if req.ID != "" {
		dbConn.Exec("UPDATE messages SET status='read' WHERE id=?", req.ID)
	} else if req.JID != "" {
		dbConn.Exec("UPDATE messages SET status='read' WHERE chat_jid=? AND status != 'read'", req.JID)
	}

	// 2. Kirim sinyal ke server WhatsApp
	if req.JID != "" {
		targetJID, err := types.ParseJID(req.JID)
		if err == nil {
			msgIDs := []types.MessageID{}
			if req.ID != "" {
				msgIDs = append(msgIDs, types.MessageID(req.ID))
			}

			// Perbaikan parameter MarkRead: Tambahkan context.Background() di awal
			errMark := waClient.MarkRead(context.Background(), msgIDs, time.Now(), targetJID, types.EmptyJID)
			if errMark != nil {
				log.Printf("⚠️ Gagal kirim MarkRead: %v", errMark)
			}
		}
	}

	return c.JSON(fiber.Map{"status": "success", "updated": true})
}

func searchMessagesGET(c *fiber.Ctx) error {
	return performSearch(c, c.Query("q"), c.Query("jid"))
}

func searchMessagesPOST(c *fiber.Ctx) error {
	var body struct {
		Q   string `json:"q"`
		JID string `json:"jid"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid JSON"})
	}
	return performSearch(c, body.Q, body.JID)
}

func performSearch(c *fiber.Ctx, searchText string, filterJid string) error {
	if searchText == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Query required"})
	}
	sqlQuery := `
		SELECT id, chat_jid, sender_name, content, is_from_me, status, timestamp FROM (
			SELECT id, chat_jid, COALESCE(sender_name, '') as sender_name, content, is_from_me, status, timestamp FROM messages 
			UNION ALL
			SELECT id, chat_id as chat_jid, COALESCE(sender_name, '') as sender_name, message as content, 0 as is_from_me, 'read' as status, created_at as timestamp FROM group_chat_logs 
		) WHERE content LIKE ?`
	
	params := []interface{}{"%" + searchText + "%"}
	if filterJid != "" {
		sqlQuery += " AND chat_jid = ?"
		params = append(params, filterJid)
	}
	sqlQuery += " ORDER BY timestamp DESC LIMIT 50"

	rows, err := dbConn.Query(sqlQuery, params...)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	results := []Msg{}
	for rows.Next() {
		var m Msg
		var ts string
		var isMeInt int
		rows.Scan(&m.ID, &m.ChatJID, &m.SenderName, &m.Content, &isMeInt, &m.Status, &ts)
		tObj, _ := time.Parse("2006-01-02 15:04:05", ts)
		m.IsMe = (isMeInt == 1)
		m.RawTime = ts
		m.Timestamp = tObj.Format("15:04")
		results = append(results, m)
	}
	return c.JSON(results)
}

func getContacts(c *fiber.Ctx) error {
	query := `
        SELECT chat_jid, sender_name, content, timestamp FROM (
            SELECT chat_jid, sender_name, content, timestamp FROM messages 
            WHERE rowid IN (SELECT MAX(rowid) FROM messages GROUP BY chat_jid)
            UNION ALL
            SELECT chat_id as chat_jid, sender_name, message as content, created_at as timestamp FROM group_chat_logs
            WHERE id IN (SELECT MAX(id) FROM group_chat_logs GROUP BY chat_id)
        ) GROUP BY chat_jid ORDER BY timestamp DESC`
	
	rows, err := dbConn.Query(query)
	if err != nil {
		return c.JSON([]interface{}{})
	}
	defer rows.Close()

	result := []map[string]interface{}{}
	for rows.Next() {
		var j, n, ct, ts string
		rows.Scan(&j, &n, &ct, &ts)
		tObj, _ := time.Parse("2006-01-02 15:04:05", ts)
		result = append(result, map[string]interface{}{
			"jid": j, "sender_name": n, "last_msg": ct, "time": tObj.Format("15:04"),
		})
	}
	return c.JSON(result)
}

func getMessages(c *fiber.Ctx) error {
	jid := c.Params("jid")
	isGroup := strings.Contains(jid, "@g.us")
	msgs := []Msg{}
	var rows *sql.Rows
	var err error

	if isGroup {
		rows, err = dbConn.Query(`SELECT id, chat_id, COALESCE(sender_name, ''), message, created_at 
			FROM group_chat_logs WHERE chat_id=? ORDER BY id DESC LIMIT 50`, jid)
	} else {
		rows, err = dbConn.Query(`SELECT id, chat_jid, COALESCE(sender_name, ''), content, is_from_me, status, timestamp 
			FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT 50`, jid)
	}

	if err != nil {
		return c.Status(500).JSON(msgs)
	}
	defer rows.Close()

	for rows.Next() {
		var m Msg
		var ts string
		var isMeInt int
		if isGroup {
			var id int64
			rows.Scan(&id, &m.ChatJID, &m.SenderName, &m.Content, &ts)
			m.ID = id
			m.IsMe = false
			m.Status = "read"
		} else {
			rows.Scan(&m.ID, &m.ChatJID, &m.SenderName, &m.Content, &isMeInt, &m.Status, &ts)
			m.IsMe = (isMeInt == 1)
		}
		tObj, _ := time.Parse("2006-01-02 15:04:05", ts)
		m.RawTime = ts
		m.Timestamp = tObj.Format("15:04")
		msgs = append(msgs, m)
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return c.JSON(msgs)
}

func sendMessage(c *fiber.Ctx) error {
	var req struct {
		JID string `json:"jid"`
		Msg string `json:"message"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).SendString("Invalid payload")
	}

	target, _ := types.ParseJID(req.JID)
	resp, err := waClient.SendMessage(context.Background(), target, &waProto.Message{Conversation: proto.String(req.Msg)})
	if err != nil {
		return c.Status(500).SendString(err.Error())
	}

	saveAndBroadcast(req.JID, "Anda", req.Msg, true, "sent", time.Now(), resp.ID)
	return c.JSON(fiber.Map{"status": "ok", "id": resp.ID})
}

// --- HELPERS ---

func saveAndBroadcast(jid, name, content string, isMe bool, status string, ts time.Time, msgID string) {
	tsStr := ts.Format("2006-01-02 15:04:05")
	var finalID interface{} = msgID

	if strings.Contains(jid, "@g.us") {
		res, _ := dbConn.Exec(`INSERT INTO group_chat_logs (chat_id, sender_name, message, created_at) VALUES (?, ?, ?, ?)`, jid, name, content, tsStr)
		id, _ := res.LastInsertId()
		finalID = id
	} else {
		sender := jid
		if isMe { sender = myJID }
		dbConn.Exec(`INSERT OR REPLACE INTO messages (id, chat_jid, sender_jid, sender_name, content, is_from_me, timestamp, status) 
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, msgID, jid, sender, name, content, isMe, tsStr, status)
	}

	msg := Msg{
		ID: finalID, ChatJID: jid, SenderName: name, Content: content,
		IsMe: isMe, Status: status, Timestamp: ts.Format("15:04"), RawTime: tsStr,
	}
	broadcastToWS(msg)
}

func broadcastToWS(msg Msg) {
	data, _ := json.Marshal(msg)
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for client := range clients {
		if err := client.WriteMessage(websocket.TextMessage, data); err != nil {
			client.Close()
			delete(clients, client)
		}
	}
}

func getText(m *waProto.Message) string {
	if m == nil { return "" }
	if m.GetConversation() != "" { return m.GetConversation() }
	if m.GetExtendedTextMessage() != nil { return m.GetExtendedTextMessage().GetText() }
	return ""
}

// --- FUNGSI LOGOUT PARIPURNA ---
func logoutWA(c *fiber.Ctx) error {
    if waClient == nil {
        return c.Status(500).JSON(fiber.Map{"error": "WhatsApp client tidak aktif"})
    }

    // Menggunakan context.Background() untuk mengatasi error "not enough arguments"
    err := waClient.Logout(context.Background())
    if err != nil {
        return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Gagal logout: %v", err)})
    }

    waClient.Disconnect()
    fmt.Println("🚪 Sesi WhatsApp diputuskan (Unpaired).")
    
    return c.JSON(fiber.Map{
        "status":  "success",
        "message": "Berhasil logout. Silakan scan QR baru untuk masuk kembali.",
    })
}

func wsHandler(c *websocket.Conn) {
	clientsMu.Lock()
	clients[c] = true
	clientsMu.Unlock()
	defer func() {
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
	}()
	for {
		if _, _, err := c.ReadMessage(); err != nil { break }
	}
}